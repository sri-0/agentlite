// Package opensearch persists threads and messages. Messages are stored as
// compacted AG-UI events plus the extracted text; nothing is converted.
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	oslib "github.com/opensearch-project/opensearch-go/v5"
	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
	"github.com/rs/zerolog"

	"agentlite/internal/config"
)

// Index names.
const (
	IndexThreads  = "threads"
	IndexMessages = "messages"
)

// Thread is a conversation; a sub-agent's thread carries parent_id.
type Thread struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	RootID    string    `json:"root_id"`
	Title     string    `json:"title"`
	AgentID   string    `json:"agent_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Message is one turn's record: compacted AG-UI events and the extracted text.
type Message struct {
	ID        string            `json:"id"`
	ThreadID  string            `json:"thread_id"`
	RootID    string            `json:"root_id"`
	Role      string            `json:"role"`
	Seq       int64             `json:"seq"`
	CreatedAt time.Time         `json:"created_at"`
	Text      string            `json:"text"`
	Events    []json.RawMessage `json:"events"`
}

// ErrNotFound is returned for a missing thread.
var ErrNotFound = errors.New("not found")

// Client is the OpenSearch store.
type Client struct {
	api *opensearchapi.Client
	log zerolog.Logger
}

// New connects and creates the indices if they are missing.
func New(ctx context.Context, cfg config.OpenSearch, log zerolog.Logger) (*Client, error) {
	api, err := opensearchapi.NewClient(opensearchapi.Config{Client: oslib.Config{
		Addresses: []string{cfg.URL}, Username: cfg.Username, Password: cfg.Password,
		DiscoverNodesOnStart: new(bool), // false: the configured URL is the only reachable address
	}})
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	c := &Client{api: api, log: log}
	for name, mapping := range indices {
		if err := c.ensureIndex(ctx, name, mapping); err != nil {
			return nil, err
		}
	}
	return c, nil
}

var indices = map[string]string{
	IndexThreads: `{"mappings":{"properties":{
		"id":{"type":"keyword"},"parent_id":{"type":"keyword"},"root_id":{"type":"keyword"},
		"title":{"type":"text"},"agent_id":{"type":"keyword"},"model":{"type":"keyword"},"user_id":{"type":"keyword"},
		"created_at":{"type":"date"},"updated_at":{"type":"date"}}}}`,
	IndexMessages: `{"mappings":{"properties":{
		"id":{"type":"keyword"},"thread_id":{"type":"keyword"},"root_id":{"type":"keyword"},"role":{"type":"keyword"},
		"seq":{"type":"integer"},"created_at":{"type":"date"},"text":{"type":"text"},
		"events":{"type":"object","enabled":false}}}}`,
}

func (c *Client) ensureIndex(ctx context.Context, name, mapping string) error {
	_, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: name, BodyReader: strings.NewReader(mapping)})
	if err != nil && !strings.Contains(err.Error(), "resource_already_exists_exception") {
		return fmt.Errorf("create index %s: %w", name, err)
	}
	return nil
}

func (c *Client) index(ctx context.Context, index, id string, doc any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = c.api.Document.Index(ctx, opensearchapi.IndexReq{
		Index: index, ID: id, Body: bytes.NewReader(body), Params: &opensearchapi.IndexParams{Refresh: "wait_for"},
	})
	if err != nil {
		return fmt.Errorf("index %s/%s: %w", index, id, err)
	}
	return nil
}

// SaveThread upserts a thread.
func (c *Client) SaveThread(ctx context.Context, t *Thread) error {
	return c.index(ctx, IndexThreads, t.ID, t)
}

// SaveMessage upserts a message.
func (c *Client) SaveMessage(ctx context.Context, m *Message) error {
	return c.index(ctx, IndexMessages, m.ID, m)
}

func (c *Client) search(ctx context.Context, index, query string, out any) error {
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{Indices: []string{index}, BodyReader: strings.NewReader(query)})
	if err != nil {
		return fmt.Errorf("search %s: %w", index, err)
	}
	sources := make([]json.RawMessage, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		sources = append(sources, h.Source)
	}
	raw, _ := json.Marshal(sources)
	return json.Unmarshal(raw, out)
}

// Threads lists root threads, newest first.
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	var out []Thread
	err := c.search(ctx, IndexThreads, `{"size":200,"sort":[{"updated_at":"desc"}],
		"query":{"bool":{"must_not":{"exists":{"field":"parent_id"}}}}}`, &out)
	return out, err
}

// Thread fetches one thread by id.
func (c *Client) Thread(ctx context.Context, id string) (*Thread, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.GetReq{Index: IndexThreads, ID: id})
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get thread: %w", err)
	}
	var t Thread
	if err := json.Unmarshal(resp.Source, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteThread removes a thread and its messages.
func (c *Client) DeleteThread(ctx context.Context, id string) error {
	if _, err := c.api.Document.Delete(ctx, opensearchapi.DeleteReq{Index: IndexThreads, ID: id}); err != nil && !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("delete thread: %w", err)
	}
	q := fmt.Sprintf(`{"query":{"term":{"thread_id":%q}}}`, id)
	_, err := c.api.DeleteByQuery(ctx, &opensearchapi.DeleteByQueryReq{Indices: []string{IndexMessages}, BodyReader: strings.NewReader(q)})
	if err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	return nil
}

// Messages returns up to limit messages of a thread with seq below before
// (0 means from the newest), in ascending seq order.
func (c *Client) Messages(ctx context.Context, threadID string, before int64, limit int) ([]Message, error) {
	rng := ""
	if before > 0 {
		rng = fmt.Sprintf(`,{"range":{"seq":{"lt":%d}}}`, before)
	}
	q := fmt.Sprintf(`{"size":%d,"sort":[{"seq":"desc"}],"query":{"bool":{"filter":[{"term":{"thread_id":%q}}%s]}}}`, limit, threadID, rng)
	var out []Message
	if err := c.search(ctx, IndexMessages, q, &out); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
