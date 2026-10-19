// Package userfiles keeps the files users upload for RAG: the record in
// OpenSearch, the body in S3. Only kinds a model takes as they are get in
// (text, images, audio, video and PDF); nothing is converted. A message names
// a file with #id (Resolve) and the model gets it: images and PDFs as they
// are, small text inline, anything else as a note.
package userfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"

	"agentlite/config"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// Errors for a file that cannot be kept: a request no upload can satisfy
// (an empty file, bad tags; an unknown workspace is
// opensearch.ErrUnknownWorkspace), and a kind of file that is not accepted.
var (
	ErrInvalid     = errors.New("invalid file")
	ErrUnsupported = errors.New("unsupported file type")
)

// Kinds of file, by what a model can do with them.
const (
	kindText  = "text"
	kindImage = "image"
	kindAudio = "audio"
	kindVideo = "video"
	kindPDF   = "pdf"
)

// kinds are the media types accepted other than text, by kind.
var kinds = map[string]string{
	"image/png": kindImage, "image/jpeg": kindImage, "image/webp": kindImage, "image/gif": kindImage,
	"audio/wav": kindAudio, "audio/mpeg": kindAudio, "video/mp4": kindVideo, "application/pdf": kindPDF,
}

// Store keeps files.
type Store struct {
	records *opensearch.Client
	bodies  *s3.Client
	limits  config.FilesConfig
}

// New returns a store.
func New(records *opensearch.Client, bodies *s3.Client, limits config.FilesConfig) *Store {
	return &Store{records: records, bodies: bodies, limits: limits}
}

// MaxBytes is the largest upload.
func (s *Store) MaxBytes() int64 { return int64(s.limits.MaxUploadMB) << 20 }

// Add keeps a file under a workspace ("" files it under none).
func (s *Store) Add(ctx context.Context, name, workspaceID string, tags []string, body []byte) (*opensearch.File, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: the file is empty", ErrInvalid)
	}
	tags, err := s.cleanTags(tags)
	if err != nil {
		return nil, err
	}
	if err := s.records.CheckWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}
	mediaType, err := sniff(body)
	if err != nil {
		return nil, err
	}
	f := &opensearch.File{
		ID: uuid.Must(uuid.NewV7()).String(), Name: name, MediaType: mediaType, Size: len(body),
		WorkspaceID: workspaceID, Tags: tags, Status: opensearch.FileStored, CreatedAt: time.Now(),
	}
	if err := s.bodies.Put(ctx, key(f.ID), mediaType, body); err != nil {
		return nil, err
	}
	if err := s.records.SaveFile(ctx, f); err != nil {
		_ = s.bodies.Delete(context.WithoutCancel(ctx), key(f.ID)) // also when the client has gone
		return nil, err
	}
	return f, nil
}

// Body opens a file's body; the caller closes it.
func (s *Store) Body(ctx context.Context, f *opensearch.File) (io.ReadSeekCloser, error) {
	return s.bodies.Get(ctx, key(f.ID))
}

// Delete removes a file: its body, then its record, so a failure can be
// retried; opensearch.ErrNotFound when there is none.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.records.File(ctx, id); err != nil {
		return err
	}
	if err := s.bodies.Delete(ctx, key(id)); err != nil {
		return err
	}
	return s.records.DeleteFile(ctx, id)
}

// key is where a file's body lives in S3.
func key(id string) string { return "files/" + id }

// sniff is a file's media type, from its content (mimetype). ErrUnsupported
// for any kind but text, images, audio, video and PDF, and for text that is
// not UTF-8 (a UTF-16 export, say).
func sniff(body []byte) (string, error) {
	mediaType := mimetype.Detect(body).String()
	switch k := kind(mediaType); {
	case k == "":
		return "", fmt.Errorf("%w: %s; upload text, PNG, JPEG, WebP or GIF images, WAV or MP3 audio, MP4 video or PDF",
			ErrUnsupported, mediaType)
	case k == kindText && !utf8.Valid(body):
		return "", fmt.Errorf("%w: text that is not UTF-8; save it as UTF-8", ErrUnsupported)
	}
	return mediaType, nil
}

// kind is what a media type is to the model; "" for one not accepted.
func kind(mediaType string) string {
	t, _, _ := mime.ParseMediaType(mediaType)
	if strings.HasPrefix(t, "text/") || t == "application/json" {
		return kindText
	}
	return kinds[t]
}

// cleanTags trims tags and drops blanks and repeats; more than the limit, or
// a longer tag, is invalid.
func (s *Store) cleanTags(tags []string) ([]string, error) {
	for i, t := range tags {
		tags[i] = strings.TrimSpace(t)
	}
	slices.Sort(tags)
	tags = slices.Compact(slices.DeleteFunc(tags, func(t string) bool { return t == "" }))
	long := slices.ContainsFunc(tags, func(t string) bool { return utf8.RuneCountInString(t) > s.limits.MaxTagChars })
	if len(tags) > s.limits.MaxTags || long {
		return nil, fmt.Errorf("%w: at most %d tags of at most %d characters", ErrInvalid, s.limits.MaxTags, s.limits.MaxTagChars)
	}
	return tags, nil
}
