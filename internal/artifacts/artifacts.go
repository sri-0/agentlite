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
	"slices"
	"strings"
	"time"

	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
	"agentlite/pkg/toolx"
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
	records *opensearch.Client
	bodies  *s3.Client // nil keeps bodies in the records
}

// New returns a store; bodies may be nil.
func New(records *opensearch.Client, bodies *s3.Client) *Store {
	return &Store{records: records, bodies: bodies}
}

// Save stores a new version of an artefact, keeping its creation time, and
// records the sources its body cites.
func (s *Store) Save(ctx context.Context, a *opensearch.Artifact, body string) error {
	if a.Kind != KindURL && contentTypes[a.Kind] == "" {
		return fmt.Errorf("unknown artifact kind %q; use one of %v or %s", a.Kind, Kinds(), KindURL)
	}
	now := time.Now()
	a.CreatedAt, a.UpdatedAt, a.Version = now, now, 1
	prev, err := s.records.Artifact(ctx, a.RootID, a.ID)
	switch {
	case err == nil:
		a.CreatedAt, a.Version = prev.CreatedAt, prev.Version+1
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
	if err := s.records.SaveArtifact(ctx, a); err != nil {
		return err
	}
	if cited := toolx.Citations(body); len(cited) > 0 {
		return s.records.Cite(ctx, a.RootID, "artifact:"+a.ID, cited)
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

// Delete removes a conversation's bodies; the records are deleted with the
// thread's documents.
func (s *Store) Delete(ctx context.Context, rootID string) error {
	if s.bodies == nil {
		return nil
	}
	return s.bodies.DeletePrefix(ctx, "artifacts/"+rootID+"/")
}

// key is where a body lives in S3; a new version replaces the old one.
func key(a *opensearch.Artifact) string { return "artifacts/" + a.RootID + "/" + a.ID }
