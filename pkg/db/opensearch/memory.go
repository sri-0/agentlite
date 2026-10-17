package opensearch

import (
	"context"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexMemories holds long-term memory entries with their embeddings.
const indexMemories = "memories"

// Memory is one remembered piece of conversation.
type Memory struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveMemory upserts a memory entry and returns once the next refresh makes it
// searchable, so a thread delete after it finds it.
func (c *Client) SaveMemory(ctx context.Context, m *Memory) error {
	return c.write(ctx, indexMemories, m.ID, m, opensearchapi.IndexParams{Refresh: "wait_for"})
}

// DeleteMemories deletes a thread's memory entries.
func (c *Client) DeleteMemories(ctx context.Context, threadID string) error {
	return c.deleteWhere(ctx, indexMemories, "thread_id", threadID)
}

// SearchMemories returns the k entries nearest to the vector, leaving out the
// entries of one thread (the one asking, whose turns are already in context).
func (c *Client) SearchMemories(ctx context.Context, vector []float32, k int, excludeThread string) ([]Memory, error) {
	notThread := map[string]any{"bool": map[string]any{"must_not": map[string]any{"term": map[string]any{"thread_id": excludeThread}}}}
	return search[Memory](ctx, c, indexMemories, map[string]any{
		"size": k, "_source": map[string]any{"excludes": []string{"embedding"}},
		"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"vector": vector, "k": k, "filter": notThread}}},
	})
}
