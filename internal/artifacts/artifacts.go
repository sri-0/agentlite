// Package artifacts stores what agents publish: the record in OpenSearch and
// the body in S3. A url artifact has no body; it points at a page the UI
// embeds.
package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/rs/zerolog"

	"agentlite/internal/sources"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// KindURL is an artifact that only points at a page.
const KindURL = "url"

// Kinds an artifact with a body can have, and the content type it is served with.
var contentTypes = map[string]string{
	"markdown": "text/markdown; charset=utf-8",
	"code":     "text/plain; charset=utf-8",
	"json":     "application/json",
	"csv":      "text/csv; charset=utf-8",
}

// Kinds lists the kinds an artifact with a body may have.
func Kinds() []string { return slices.Sorted(maps.Keys(contentTypes)) }

// ContentType is how a kind's body is served.
func ContentType(kind string) string { return contentTypes[kind] }

// Store keeps artifacts.
type Store struct {
	records *opensearch.Client
	bodies  *s3.Client
	sources *sources.Recorder
	log     zerolog.Logger
}

// New returns a store.
func New(records *opensearch.Client, bodies *s3.Client, sources *sources.Recorder, log zerolog.Logger) *Store {
	return &Store{records: records, bodies: bodies, sources: sources, log: log}
}

// validID is what an artifact id may be: it is part of a document id, an S3
// key and a URL.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Save stores a new version of an artifact, keeping its creation time, and
// records the sources its body cites. Versions are numbered atomically (the
// conversation's counters document), each body has its own key, and the
// record only moves forward, so two saves of one artifact at once both keep
// their versions and the later one is served.
func (s *Store) Save(ctx context.Context, a *opensearch.Artifact, body string) error {
	switch {
	case !validID.MatchString(a.ID):
		return fmt.Errorf("artifact id %q: use letters, digits, '.', '_' and '-'", a.ID)
	case a.Kind == KindURL && a.URL == "":
		return fmt.Errorf("a %s artifact needs a url", KindURL)
	case a.Kind != KindURL && contentTypes[a.Kind] == "":
		return fmt.Errorf("unknown artifact kind %q; use one of %v", a.Kind, Kinds())
	}
	now := time.Now()
	a.CreatedAt, a.UpdatedAt = now, now
	floor := 0
	prev, err := s.records.Artifact(ctx, a.RootID, a.ID)
	switch {
	case err == nil:
		a.CreatedAt, floor = prev.CreatedAt, prev.Version
	case !errors.Is(err, opensearch.ErrNotFound):
		return err
	}
	if a.Version, err = s.records.NextArtifactVersion(ctx, a.RootID, a.ID, floor); err != nil {
		return err
	}
	if a.Kind != KindURL {
		a.Size = len(body)
		if err := s.bodies.Put(ctx, key(a), contentTypes[a.Kind], []byte(body)); err != nil {
			return err
		}
	}
	// On a conflict a later version, saved at the same moment, is already the
	// record; this version's body stays in S3 unread until the thread goes.
	if err := s.records.SaveArtifact(ctx, a); err != nil && !errors.Is(err, opensearch.ErrConflict) {
		return err
	}
	// The artifact is saved; a citation that fails to record is logged, not
	// reported as a failed save the model would retry.
	if err := s.sources.Cite(ctx, a.RootID, "artifact:"+a.ID, body); err != nil {
		s.log.Warn().Err(err).Str("artifact", a.ID).Msg("record citations")
	}
	return nil
}

// Summary is an artifact as the model and the event stream see it: what it
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

// Body opens an artifact's body; opensearch.ErrNotFound for a url artifact.
func (s *Store) Body(ctx context.Context, a *opensearch.Artifact) (io.ReadCloser, error) {
	if a.Kind == KindURL {
		return nil, opensearch.ErrNotFound
	}
	return s.bodies.Get(ctx, key(a))
}

// Delete removes a conversation's bodies; the records and counters are
// deleted with the thread's documents.
func (s *Store) Delete(ctx context.Context, rootID string) error {
	return s.bodies.DeletePrefix(ctx, "artifacts/"+rootID+"/")
}

// key is where a version's body lives in S3.
func key(a *opensearch.Artifact) string {
	return fmt.Sprintf("artifacts/%s/%s/%d", a.RootID, a.ID, a.Version)
}
