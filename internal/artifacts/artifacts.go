// Package artifacts stores what agents publish: the record in OpenSearch and
// the body in S3, or in the record itself when S3 is not configured. A url
// artefact has no body; it points at a page the UI embeds.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/sources"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// KindURL is an artefact that only points at a page.
const KindURL = "url"

// Kinds an artefact with a body can have, and the content type it is served with.
var contentTypes = map[string]string{
	"markdown": "text/markdown; charset=utf-8",
	"code":     "text/plain; charset=utf-8",
	"html":     "text/html; charset=utf-8",
	"json":     "application/json",
	"csv":      "text/csv; charset=utf-8",
}

// Kinds lists the kinds an artefact with a body may have.
func Kinds() []string { return slices.Sorted(maps.Keys(contentTypes)) }

// ContentType is how a kind's body is served.
func ContentType(kind string) string { return contentTypes[kind] }

// Store keeps artefacts.
type Store struct {
	records  *opensearch.Client
	bodies   *s3.Client    // nil keeps bodies in the records
	versions valkey.Client // version counters, one hash per conversation
	sources  *sources.Recorder
	log      zerolog.Logger
}

// New returns a store; bodies may be nil.
func New(records *opensearch.Client, bodies *s3.Client, versions valkey.Client, sources *sources.Recorder, log zerolog.Logger) *Store {
	return &Store{records: records, bodies: bodies, versions: versions, sources: sources, log: log}
}

func versionsKey(rootID string) string { return "thread:" + rootID + ":artifacts" }

// validID is what an artefact id may be: it is part of a document id, an S3
// key and a URL.
var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Save stores a new version of an artefact, keeping its creation time, and
// records the sources its body cites. Versions are numbered atomically, each
// body has its own key, and the record only moves forward, so two saves of
// one artefact at once both keep their versions and the later one is served.
func (s *Store) Save(ctx context.Context, a *opensearch.Artifact, body string) error {
	switch {
	case !validID.MatchString(a.ID):
		return fmt.Errorf("artifact id %q: use letters, digits, '.', '_' and '-'", a.ID)
	case a.Kind == KindURL && a.URL == "":
		return fmt.Errorf("a %s artifact needs a url", KindURL)
	case a.Kind != KindURL && contentTypes[a.Kind] == "":
		return fmt.Errorf("unknown artifact kind %q; use one of %v", a.Kind, Kinds())
	}
	version, err := s.versions.Do(ctx, s.versions.B().Hincrby().Key(versionsKey(a.RootID)).Field(a.ID).Increment(1).Build()).AsInt64()
	if err != nil {
		return fmt.Errorf("number artifact version: %w", err)
	}
	now := time.Now()
	a.Version, a.CreatedAt, a.UpdatedAt = int(version), now, now
	prev, err := s.records.Artifact(ctx, a.RootID, a.ID)
	switch {
	case err == nil:
		a.CreatedAt = prev.CreatedAt
	case !errors.Is(err, opensearch.ErrNotFound):
		return err
	}
	if a.Kind != KindURL {
		a.Size = len(body)
		if s.bodies != nil {
			if err := s.bodies.Put(ctx, key(a), contentTypes[a.Kind], []byte(body)); err != nil {
				return err
			}
			a.Stored = opensearch.StoredS3
		} else {
			a.Stored, a.Content = opensearch.StoredInline, body
		}
	}
	if err := s.records.SaveArtifact(ctx, a); err != nil && !errors.Is(err, opensearch.ErrConflict) {
		return err // on a conflict a later version is already stored; this body stays as history
	}
	// The artefact is saved; a citation that fails to record is logged, not
	// reported as a failed save the model would retry.
	if err := s.sources.Cite(ctx, a.RootID, "artifact:"+a.ID, body); err != nil {
		s.log.Warn().Err(err).Str("artifact", a.ID).Msg("record citations")
	}
	return nil
}

// Summary is an artefact as the model and the event stream see it: what it
// is and where, never its body. The UI fetches the body from the API.
func Summary(a *opensearch.Artifact) map[string]any {
	out := map[string]any{"id": a.ID, "title": a.Title, "kind": a.Kind, "version": a.Version}
	if a.Language != "" {
		out["language"] = a.Language
	}
	if a.URL != "" {
		out["url"] = a.URL
	}
	return out
}

// Body opens an artefact's body; opensearch.ErrNotFound for a url artefact.
func (s *Store) Body(ctx context.Context, a *opensearch.Artifact) (io.ReadCloser, error) {
	switch a.Stored {
	case opensearch.StoredS3:
		if s.bodies == nil {
			return nil, fmt.Errorf("artifact %s is in S3, which is not configured", a.ID)
		}
		return s.bodies.Get(ctx, key(a))
	case opensearch.StoredInline:
		return io.NopCloser(strings.NewReader(a.Content)), nil
	}
	return nil, opensearch.ErrNotFound
}

// Delete removes a conversation's bodies and version counters; the records
// are deleted with the thread's documents.
func (s *Store) Delete(ctx context.Context, rootID string) error {
	if s.bodies != nil {
		if err := s.bodies.DeletePrefix(ctx, "artifacts/"+rootID+"/"); err != nil {
			return err
		}
	}
	return s.versions.Do(ctx, s.versions.B().Del().Key(versionsKey(rootID)).Build()).Error()
}

// key is where a version's body lives in S3.
func key(a *opensearch.Artifact) string {
	return fmt.Sprintf("artifacts/%s/%s/%d", a.RootID, a.ID, a.Version)
}
