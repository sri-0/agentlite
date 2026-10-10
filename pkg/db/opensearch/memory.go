package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// IndexMemories holds long-term memory entries with their embeddings.
const IndexMemories = "memories"

// Memory is one remembered piece of conversation.
type Memory struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	Embedding []float32 `json:"embedding,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EnsureMemoryIndex creates the kNN index for the embedding size in use.
func (c *Client) EnsureMemoryIndex(ctx context.Context, dimensions int) error {
	mapping := fmt.Sprintf(`{"settings":{"index.knn":true},"mappings":{"properties":{
		"id":{"type":"keyword"},"thread_id":{"type":"keyword"},"author":{"type":"keyword"},
		"text":{"type":"text"},"created_at":{"type":"date"},
		"embedding":{"type":"knn_vector","dimension":%d,"method":{"name":"hnsw","space_type":"cosinesimil","engine":"lucene"}}}}}`, dimensions)
	return c.ensureIndex(ctx, IndexMemories, mapping)
}

// SaveMemory upserts a memory entry.
func (c *Client) SaveMemory(ctx context.Context, m *Memory) error {
	return c.index(ctx, IndexMemories, m.ID, m)
}

// SearchMemories returns the k entries nearest to the vector, leaving out the
// entries of one thread (the one asking, whose turns are already in context).
func (c *Client) SearchMemories(ctx context.Context, vector []float32, k int, excludeThread string) ([]Memory, error) {
	vec, _ := json.Marshal(vector)
	q := fmt.Sprintf(`{"size":%d,"_source":{"excludes":["embedding"]},"query":{"knn":{"embedding":{"vector":%s,"k":%d,
		"filter":{"bool":{"must_not":{"term":{"thread_id":%q}}}}}}}}`, k, vec, k, excludeThread)
	return search[Memory](ctx, c, IndexMemories, q)
}
