package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexFiles holds the records of the files users uploaded for RAG; their
// bodies are in S3 (internal/files).
const indexFiles = "userfiles"

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
	if workspaceID != nil {
		q["query"] = inWorkspace(*workspaceID)
	}
	return search[File](ctx, c, indexFiles, q)
}

// inWorkspace matches the files (or chunks) of one workspace; "" matches
// the unfiled ones.
func inWorkspace(id string) map[string]any {
	if id == "" {
		return map[string]any{"bool": map[string]any{"must_not": map[string]any{"exists": map[string]any{"field": "workspace_id"}}}}
	}
	return map[string]any{"term": map[string]any{"workspace_id": id}}
}

// DeleteFile removes a file's record; ErrNotFound when there is none.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	return c.delete(ctx, indexFiles, id)
}

// indexFileChunks holds what files hold, embedded for search: a text file's
// chunks, an image, audio or video whole.
const indexFileChunks = "file_chunks"

// FileChunk is one embedded piece of a file. It repeats the file's name,
// media type and workspace, so a search needs no second lookup.
type FileChunk struct {
	ID          string    `json:"id"` // <file id>:<seq>
	FileID      string    `json:"file_id"`
	Name        string    `json:"name"`
	MediaType   string    `json:"media_type"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
	Seq         int       `json:"seq"`
	Text        string    `json:"text,omitempty"` // a text file's chunk
	Embedding   []float32 `json:"embedding,omitempty"`
}

// SaveFileChunks stores a file's chunks in one bulk request, readable by
// searches when it returns.
func (c *Client) SaveFileChunks(ctx context.Context, chunks []FileChunk) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, ch := range chunks {
		if err := enc.Encode(map[string]any{"index": map[string]any{"_index": c.ix(indexFileChunks), "_id": ch.ID}}); err != nil {
			return err
		}
		if err := enc.Encode(ch); err != nil {
			return err
		}
	}
	resp, err := c.api.Bulk(ctx, opensearchapi.BulkReq{Body: &body, Params: &opensearchapi.BulkParams{Refresh: "true"}})
	if err != nil {
		return fmt.Errorf("bulk %s: %w", indexFileChunks, err)
	}
	for _, item := range resp.Items {
		if r := item.Index; r != nil && r.Error != nil {
			cause, _ := json.Marshal(r.Error)
			return fmt.Errorf("bulk %s: %s", indexFileChunks, cause)
		}
	}
	return nil
}

// SearchFileChunks returns the k chunks nearest to the vector, best first,
// of one workspace's files ("" for the unfiled ones), and of those only the
// given files when fileIDs is not empty.
func (c *Client) SearchFileChunks(
	ctx context.Context, vector []float32, k int, workspaceID string, fileIDs []string,
) ([]FileChunk, error) {
	filter := inWorkspace(workspaceID)
	if len(fileIDs) > 0 {
		filter = map[string]any{"bool": map[string]any{"filter": []any{
			filter, map[string]any{"terms": map[string]any{"file_id": fileIDs}},
		}}}
	}
	return search[FileChunk](ctx, c, indexFileChunks, map[string]any{
		"size": k, "_source": map[string]any{"excludes": []string{"embedding"}},
		"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{
			"vector": vector, "k": max(k, knnCandidates), "filter": filter,
		}}},
	})
}

// knnCandidates is how many neighbours HNSW looks for before the best k are
// kept: asked for only k, it missed the best chunk among near-alike ones
// (the passage that differs, in a long report of the same paragraphs).
const knnCandidates = 100

// DeleteFileChunks removes a file's chunks.
func (c *Client) DeleteFileChunks(ctx context.Context, fileID string) error {
	return c.deleteWhere(ctx, indexFileChunks, "file_id", fileID)
}
