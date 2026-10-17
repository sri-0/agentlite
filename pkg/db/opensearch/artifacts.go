package opensearch

import (
	"context"
	"time"

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

func artifactID(rootID, id string) string { return rootID + ":" + id }

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

// Key is the artifact's place in RecentArtifacts' order.
func (a *Artifact) Key() ArtifactKey {
	return ArtifactKey{UpdatedAt: a.UpdatedAt.UnixMilli(), RootID: a.RootID, ID: a.ID}
}

// RecentArtifacts lists artifacts across every conversation: up to limit,
// after the given one (nil: from the newest).
func (c *Client) RecentArtifacts(ctx context.Context, after *ArtifactKey, limit int) ([]Artifact, error) {
	body := opensearchapi.SearchBody{Size: new(limit), Sort: sortBy("-updated_at", "root_id", "id")}
	if after != nil {
		body.SearchAfter = []opensearchapi.FieldValue{
			opensearchapi.NewFieldValueFromFloat64(float64(after.UpdatedAt)),
			opensearchapi.NewFieldValueFromString(after.RootID), opensearchapi.NewFieldValueFromString(after.ID),
		}
	}
	return search[Artifact](ctx, c, indexArtifacts, body)
}

// Artifacts lists a conversation's artifacts, newest first.
func (c *Client) Artifacts(ctx context.Context, rootID string) ([]Artifact, error) {
	return search[Artifact](ctx, c, indexArtifacts, opensearchapi.SearchBody{
		Size: new(listCap), Sort: sortBy("-updated_at"), Query: new(term("root_id", rootID)),
	})
}
