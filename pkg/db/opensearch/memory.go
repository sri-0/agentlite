package opensearch

import (
	"context"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexEmbeddings holds every embedded thing, for search: long-term memory
// entries and what uploaded files hold, told apart by kind. They share it
// because one model embeds both (models.yaml's embedding block).
const indexEmbeddings = "embeddings"

// Kinds of what the embeddings index holds.
const (
	kindMemory = "memory"
	kindFile   = "file"
)

// ofKind matches the embeddings of one kind.
func ofKind(kind string) map[string]any { return map[string]any{"term": map[string]any{"kind": kind}} }

// Memory is one remembered piece of conversation.
type Memory struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // memory
	ThreadID  string    `json:"thread_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveMemory upserts a memory entry. Nothing reads one back at once, so the
// index refreshes on its own schedule.
func (c *Client) SaveMemory(ctx context.Context, m *Memory) error {
	m.Kind = kindMemory
	return c.write(ctx, indexEmbeddings, m.ID, m, opensearchapi.IndexParams{})
}

// DeleteMemories deletes a thread's memory entries.
func (c *Client) DeleteMemories(ctx context.Context, threadID string) error {
	return c.deleteWhere(ctx, indexEmbeddings, "thread_id", threadID) // only memories have one
}

// SearchMemories returns the k entries nearest to the vector, leaving out the
// entries of one thread (the one asking, whose turns are already in context).
func (c *Client) SearchMemories(ctx context.Context, vector []float32, k int, excludeThread string) ([]Memory, error) {
	notThread := map[string]any{"bool": map[string]any{
		"filter":   ofKind(kindMemory),
		"must_not": map[string]any{"term": map[string]any{"thread_id": excludeThread}},
	}}
	return search[Memory](ctx, c, indexEmbeddings, map[string]any{
		"size": k, "_source": map[string]any{"excludes": []string{"embedding"}},
		"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"vector": vector, "k": k, "filter": notThread}}},
	})
}
