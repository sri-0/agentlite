package opensearch

import (
	"context"
	"time"
)

// indexFiles holds the records of the files users uploaded; their bodies are
// in S3 (internal/files).
const indexFiles = "files"

// File statuses: a ready file is embedded and searchable, a stored one is
// kept and can be referenced but is not searchable (a PDF, or any file when
// no embedding model is configured).
const (
	FileReady  = "ready"
	FileStored = "stored"
)

// File is a file a user uploaded, filed under a workspace or none.
type File struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MediaType   string    `json:"media_type"`
	Size        int       `json:"size"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Tags        []string  `json:"tags,omitempty"` // the uploader's, for finding it
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// SaveFile upserts a file's record.
func (c *Client) SaveFile(ctx context.Context, f *File) error {
	return c.index(ctx, indexFiles, f.ID, f)
}

// File fetches one file's record; ErrNotFound when there is none.
func (c *Client) File(ctx context.Context, id string) (*File, error) {
	return get[File](ctx, c, indexFiles, id)
}

// Files lists files, newest first: every file when workspaceID is nil, the
// unfiled ones when it is "", else one workspace's.
func (c *Client) Files(ctx context.Context, workspaceID *string) ([]File, error) {
	q := map[string]any{"size": listCap, "sort": []any{map[string]any{"created_at": "desc"}}}
	switch {
	case workspaceID == nil:
	case *workspaceID == "":
		q["query"] = map[string]any{"bool": map[string]any{"must_not": map[string]any{"exists": map[string]any{"field": "workspace_id"}}}}
	default:
		q["query"] = map[string]any{"term": map[string]any{"workspace_id": *workspaceID}}
	}
	return search[File](ctx, c, indexFiles, q)
}

// DeleteFile removes a file's record; ErrNotFound when there is none.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	return c.delete(ctx, indexFiles, id)
}
