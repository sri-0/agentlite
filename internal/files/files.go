// Package files keeps the files users upload: the record in OpenSearch, the
// body in S3, and, with an embedding model, what the file holds embedded for
// search_userfiles. Only kinds a model takes as they are get in (text,
// images, audio, video and PDF); nothing is converted. A message names a
// file with #id (Resolve) and the model gets it: images and PDFs as they
// are, small text inline, anything else as a note.
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// MaxBytes is the largest file accepted.
const MaxBytes = 25 << 20

// maxName bounds a file's name, which the UI shows whole.
const maxName = 200

// Errors for a file that cannot be kept: a request no upload can satisfy
// (an empty file, an unknown workspace, a text too big to search), a kind of
// file that is not accepted, and one the embedding model refused or failed.
var (
	ErrInvalid     = errors.New("invalid file")
	ErrUnsupported = errors.New("unsupported file type")
	ErrEmbed       = errors.New("the embedding model could not embed the file")
)

// Kinds of file, by what a model can do with them.
const (
	kindText  = "text"
	kindImage = "image"
	kindAudio = "audio"
	kindVideo = "video"
	kindPDF   = "pdf"
)

// Store keeps files.
type Store struct {
	records  *opensearch.Client
	bodies   *s3.Client
	embedder *Embedder // nil: files are kept, not searchable
}

// New returns a store; with an embedder, files are searchable.
func New(records *opensearch.Client, bodies *s3.Client, embedder *Embedder) *Store {
	return &Store{records: records, bodies: bodies, embedder: embedder}
}

// Add keeps a file under a workspace ("" files it under none), embedded for
// search first: a file the model refuses is not kept.
func (s *Store) Add(ctx context.Context, name, workspaceID string, body []byte) (*opensearch.File, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: the file is empty", ErrInvalid)
	}
	if workspaceID != "" {
		_, err := s.records.Workspace(ctx, workspaceID)
		if errors.Is(err, opensearch.ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown workspace %q", ErrInvalid, workspaceID)
		}
		if err != nil {
			return nil, err
		}
	}
	mediaType, err := sniff(name, body)
	if err != nil {
		return nil, err
	}
	f := &opensearch.File{
		ID: uuid.Must(uuid.NewV7()).String(), Name: cleanName(name), MediaType: mediaType, Size: len(body),
		WorkspaceID: workspaceID, Status: opensearch.FileStored, CreatedAt: time.Now(),
	}
	chunks, err := s.index(ctx, f, body)
	if err != nil {
		return nil, err
	}
	if len(chunks) > 0 {
		f.Status = opensearch.FileReady
	}
	// The record goes last: a file is listed only once its body and chunks are in.
	if err := s.bodies.Put(ctx, key(f.ID), mediaType, body); err != nil {
		return nil, err
	}
	if len(chunks) > 0 {
		if err := s.records.SaveFileChunks(ctx, chunks); err != nil {
			_ = s.remove(ctx, f.ID)
			return nil, err
		}
	}
	if err := s.records.SaveFile(ctx, f); err != nil {
		_ = s.remove(ctx, f.ID)
		return nil, err
	}
	return f, nil
}

// Body opens a file's body; the caller closes it.
func (s *Store) Body(ctx context.Context, f *opensearch.File) (io.ReadCloser, error) {
	return s.bodies.Get(ctx, key(f.ID))
}

// Delete removes a file: its body and chunks, then its record, so a failure
// can be retried; opensearch.ErrNotFound when there is none.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.records.File(ctx, id); err != nil {
		return err
	}
	if err := s.remove(ctx, id); err != nil {
		return err
	}
	return s.records.DeleteFile(ctx, id)
}

// remove deletes what a file has besides its record: its body and chunks.
func (s *Store) remove(ctx context.Context, id string) error {
	if err := s.bodies.Delete(ctx, key(id)); err != nil {
		return err
	}
	return s.records.DeleteFileChunks(ctx, id)
}

// read is a file's whole body.
func (s *Store) read(ctx context.Context, f *opensearch.File) ([]byte, error) {
	body, err := s.Body(ctx, f)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// key is where a file's body lives in S3.
func key(id string) string { return "files/" + id }

// textTypes refine plain text by extension; the first bytes cannot tell
// Markdown, CSV or JSON from other text.
var textTypes = map[string]string{
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".csv":      "text/csv; charset=utf-8",
	".json":     "application/json",
}

// sniff is a file's media type, from its first bytes (http.DetectContentType)
// and, for plain text and an MP3 without an ID3 tag, its extension.
// ErrUnsupported for any kind but text, images, audio, video and PDF.
func sniff(name string, body []byte) (string, error) {
	mediaType := http.DetectContentType(body)
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case strings.HasPrefix(mediaType, "text/plain") && textTypes[ext] != "":
		mediaType = textTypes[ext]
	case mediaType == "application/octet-stream" && ext == ".mp3":
		mediaType = "audio/mpeg"
	}
	if kind(mediaType) == "" {
		return "", fmt.Errorf("%w: %s; upload text, PNG, JPEG, WebP or GIF images, WAV or MP3 audio, MP4 video or PDF",
			ErrUnsupported, base(mediaType))
	}
	return mediaType, nil
}

// kind is what a media type is to the model; "" for one not accepted.
func kind(mediaType string) string {
	switch t := base(mediaType); {
	case strings.HasPrefix(t, "text/"), t == "application/json":
		return kindText
	case t == "image/png", t == "image/jpeg", t == "image/webp", t == "image/gif":
		return kindImage
	case t == "audio/wave", t == "audio/mpeg":
		return kindAudio
	case t == "video/mp4":
		return kindVideo
	case t == "application/pdf":
		return kindPDF
	}
	return ""
}

// base is a media type without its parameters: text/plain, not
// text/plain; charset=utf-8.
func base(mediaType string) string {
	t, _, _ := strings.Cut(mediaType, ";")
	return t
}

// cleanName is the name a file is listed under: the upload's own name
// without a path, cut to maxName runes.
func cleanName(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	if utf8.RuneCountInString(name) > maxName {
		return string([]rune(name)[:maxName])
	}
	return name
}
