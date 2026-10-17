package opensearch

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexArtifacts holds the artifacts conversations published.
const indexArtifacts = "artifacts"

// Artifact is a published document or an embedded page. Its body is in S3,
// or nowhere for a url artifact, which only points at a page.
type Artifact struct {
	ID        string    `json:"id"` // as the agent names it; unique within a conversation
	RootID    string    `json:"root_id"`
	ThreadID  string    `json:"thread_id"`
	RunID     string    `json:"run_id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"` // markdown, code, json, csv or url
	Language  string    `json:"language,omitempty"`
	URL       string    `json:"url,omitempty"` // kind url: the page to embed
	Size      int       `json:"size"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// artifactID is an artifact's document id: a name-based UUID of its
// conversation and the name the agent gave it.
func artifactID(rootID, id string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(rootID+"\x00"+id)).String()
}

// SaveArtifact stores a version of an artifact under its conversation. The
// record only moves forward: when a later version is already stored, it is
// kept and ErrConflict returned.
func (c *Client) SaveArtifact(ctx context.Context, a *Artifact) error {
	return c.write(ctx, indexArtifacts, artifactID(a.RootID, a.ID), a,
		opensearchapi.IndexParams{Version: &a.Version, VersionType: opensearchapi.VersionTypeExternal, Refresh: "true"})
}

// Artifact fetches one artifact of a conversation.
func (c *Client) Artifact(ctx context.Context, rootID, id string) (*Artifact, error) {
	return get[Artifact](ctx, c, indexArtifacts, artifactID(rootID, id))
}

// ArtifactKey is an artifact's place in RecentArtifacts' order: newest
// first, ties broken by conversation and id, so a page never skips one.
type ArtifactKey struct {
	UpdatedAt int64  `json:"t"` // Unix milliseconds, as the index stores it
	RootID    string `json:"r"`
	ID        string `json:"i"`
}

// RecentArtifacts lists a page of artifacts across every conversation (see
// pageSize), after the given one (nil: from the newest). next is where the
// following page starts; nil when this one was the last.
func (c *Client) RecentArtifacts(ctx context.Context, after *ArtifactKey, limit int) (found []Artifact, next *ArtifactKey, err error) {
	size := pageSize(limit)
	q := map[string]any{
		"size": size,
		"sort": []any{map[string]any{"updated_at": "desc"}, map[string]any{"root_id": "asc"}, map[string]any{"id": "asc"}},
	}
	if after != nil {
		q["search_after"] = []any{after.UpdatedAt, after.RootID, after.ID}
	}
	found, err = search[Artifact](ctx, c, indexArtifacts, q)
	if err != nil || len(found) < size {
		return found, nil, err
	}
	last := found[len(found)-1]
	return found, &ArtifactKey{UpdatedAt: last.UpdatedAt.UnixMilli(), RootID: last.RootID, ID: last.ID}, nil
}

// Artifacts lists a conversation's artifacts, newest first.
func (c *Client) Artifacts(ctx context.Context, rootID string) ([]Artifact, error) {
	return search[Artifact](ctx, c, indexArtifacts, map[string]any{
		"size": listCap, "sort": []any{map[string]any{"updated_at": "desc"}}, "query": map[string]any{"term": map[string]any{"root_id": rootID}},
	})
}
