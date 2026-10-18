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

// FileChunk is one embedded piece of a file, in the embeddings index: a text
// file's chunk, or an image, audio or video whole. It repeats the file's
// name, media type and workspace, so a search needs no second lookup.
type FileChunk struct {
	ID          string    `json:"id"`   // <file id>:<seq>
	Kind        string    `json:"kind"` // file
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
		ch.Kind = kindFile
		if err := enc.Encode(map[string]any{"index": map[string]any{"_index": c.ix(indexEmbeddings), "_id": ch.ID}}); err != nil {
			return err
		}
		if err := enc.Encode(ch); err != nil {
			return err
		}
	}
	resp, err := c.api.Bulk(ctx, opensearchapi.BulkReq{Body: &body, Params: &opensearchapi.BulkParams{Refresh: "true"}})
	if err != nil {
		return fmt.Errorf("bulk %s: %w", indexEmbeddings, err)
	}
	for _, item := range resp.Items {
		if r := item.Index; r != nil && r.Error != nil {
			cause, _ := json.Marshal(r.Error)
			return fmt.Errorf("bulk %s: %s", indexEmbeddings, cause)
		}
	}
	return nil
}

// SearchFileChunks returns the k chunks nearest to the vector, best first:
// of the given files when fileIDs is not empty, else of one workspace's files
// ("" for the unfiled ones), else, with workspaceID nil, of every file.
func (c *Client) SearchFileChunks(
	ctx context.Context, vector []float32, k int, workspaceID *string, fileIDs []string,
) ([]FileChunk, error) {
	filters := []any{ofKind(kindFile)}
	switch {
	case len(fileIDs) > 0:
		filters = append(filters, map[string]any{"terms": map[string]any{"file_id": fileIDs}})
	case workspaceID != nil:
		filters = append(filters, inWorkspace(*workspaceID))
	}
	filter := map[string]any{"bool": map[string]any{"filter": filters}}
	return search[FileChunk](ctx, c, indexEmbeddings, map[string]any{
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
	return c.deleteWhere(ctx, indexEmbeddings, "file_id", fileID) // only file chunks have one
}
