package opensearch

import (
	"context"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// IndexArtifacts holds the artefacts conversations published.
const IndexArtifacts = "artifacts"

// Artifact is a published document or an embedded page. Its body is in S3
// (Stored "s3"), in Content (Stored "inline", when S3 is not configured), or
// nowhere for a url artefact, which only points at a page.
type Artifact struct {
	ID        string    `json:"id"` // as the agent names it; unique within a conversation
	RootID    string    `json:"root_id"`
	ThreadID  string    `json:"thread_id"`
	RunID     string    `json:"run_id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"` // markdown, code, html, json, csv or url
	Language  string    `json:"language,omitempty"`
	URL       string    `json:"url,omitempty"` // kind url: the page to embed
	Size      int       `json:"size"`
	Stored    string    `json:"stored,omitempty"`
	Content   string    `json:"content,omitempty"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Where an artefact's body is kept.
const (
	StoredS3     = "s3"
	StoredInline = "inline"
)

const artifactsMapping = `{"mappings":{"properties":{
	"id":{"type":"keyword"},"root_id":{"type":"keyword"},"thread_id":{"type":"keyword"},"run_id":{"type":"keyword"},
	"title":{"type":"text"},"kind":{"type":"keyword"},"language":{"type":"keyword"},"url":{"type":"keyword"},
	"size":{"type":"long"},"stored":{"type":"keyword"},"content":{"type":"text","index":false},
	"version":{"type":"integer"},"created_at":{"type":"date"},"updated_at":{"type":"date"}}}}`

func artifactID(rootID, id string) string { return rootID + ":" + id }

// SaveArtifact stores a version of an artefact under its conversation. The
// record only moves forward: when a later version is already stored, it is
// kept and ErrConflict returned.
func (c *Client) SaveArtifact(ctx context.Context, a *Artifact) error {
	version := a.Version
	return c.write(ctx, IndexArtifacts, artifactID(a.RootID, a.ID), a,
		opensearchapi.IndexParams{Version: &version, VersionType: opensearchapi.VersionTypeExternal})
}

// Artifact fetches one artefact of a conversation, body included when inline.
func (c *Client) Artifact(ctx context.Context, rootID, id string) (*Artifact, error) {
	return get[Artifact](ctx, c, IndexArtifacts, artifactID(rootID, id))
}

// Artifacts lists a conversation's artefacts, newest first, without bodies.
func (c *Client) Artifacts(ctx context.Context, rootID string) ([]Artifact, error) {
	return search[Artifact](ctx, c, IndexArtifacts, fmt.Sprintf(`{"size":%d,"sort":[{"updated_at":"desc"}],
		"_source":{"excludes":["content"]},"query":{"term":{"root_id":%q}}}`, listCap, rootID))
}
