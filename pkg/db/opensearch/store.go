// Package opensearch persists threads, messages, sources and artifacts.
// Messages are stored as coalesced AG-UI events plus the extracted text;
// nothing is converted. The indices and their mappings are created outside
// the service, from deploy/opensearch (make indices).
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	oslib "github.com/opensearch-project/opensearch-go/v5"
	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// Config is the OpenSearch connection.
type Config struct {
	URL      string `env:"URL, default=http://localhost:9200"`
	Username string `env:"USERNAME"`
	Password string `env:"PASSWORD"`
}

// Index names.
const (
	indexThreads  = "threads"
	indexMessages = "messages"
)

// Thread is a conversation; a sub-agent's thread carries parent_id.
type Thread struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	RootID   string `json:"root_id"`
	Title    string `json:"title"`
	AgentID  string `json:"agent_id,omitempty"`
	Model    string `json:"model,omitempty"`
	// Connectors switched on for the thread; nil means every connector.
	Connectors *[]string `json:"connectors,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
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

// Errors the store returns for a missing document, and for creating one
// whose id is taken.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("write conflict")
)

// Client is the OpenSearch store.
type Client struct {
	api *opensearchapi.Client
}

// New returns a client of the cluster.
func New(cfg Config) (*Client, error) {
	api, err := opensearchapi.NewClient(opensearchapi.Config{Client: oslib.Config{
		Addresses: []string{cfg.URL}, Username: cfg.Username, Password: cfg.Password,
		DiscoverNodesOnStart: new(bool), // false: the configured URL is the only reachable address
	}})
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	return &Client{api: api}, nil
}

// pageSize is how many entries a page of a list holds: 50 unless the client
// asks for another number, and at most 200.
func pageSize(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

// listCap bounds the lists of one conversation's sources and artifacts, and
// the skills library.
const listCap = 1000

// refreshed makes a write readable by searches as soon as it returns.
var refreshed = opensearchapi.IndexParams{Refresh: "true"}

// index upserts a document and refreshes the index at once: writes are few,
// and a thread or message must be readable as soon as its save returns.
func (c *Client) index(ctx context.Context, index, id string, doc any) error {
	return c.write(ctx, index, id, doc, refreshed)
}

// write stores a document. A write the params forbid (create of a taken id,
// an external version not above the stored one) keeps the stored document
// and returns ErrConflict.
func (c *Client) write(ctx context.Context, index, id string, doc any, params opensearchapi.IndexParams) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = c.api.Document.Index(ctx, opensearchapi.IndexReq{Index: index, ID: id, Body: bytes.NewReader(body), Params: &params})
	if errStatus(err) == 409 {
		return fmt.Errorf("index %s/%s: %w", index, id, ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("index %s/%s: %w", index, id, err)
	}
	return nil
}

// errStatus is the HTTP status of an API error, or 0.
func errStatus(err error) int {
	if e, ok := errors.AsType[*oslib.StructError](err); ok {
		return e.Status
	}
	if e, ok := errors.AsType[*oslib.StringError](err); ok {
		return e.Status
	}
	return 0
}

// SaveThread upserts a thread.
func (c *Client) SaveThread(ctx context.Context, t *Thread) error {
	return c.index(ctx, indexThreads, t.ID, t)
}

// SaveMessage upserts a message.
func (c *Client) SaveMessage(ctx context.Context, m *Message) error {
	return c.index(ctx, indexMessages, m.ID, m)
}

// Threads lists root threads, newest first; the list is capped at threadsCap.
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	return search[Thread](ctx, c, indexThreads, map[string]any{
		"size": threadsCap, "sort": []any{map[string]any{"updated_at": "desc"}},
		"query": map[string]any{"bool": map[string]any{"must_not": map[string]any{"exists": map[string]any{"field": "parent_id"}}}},
	})
}

const threadsCap = 200

// Thread fetches one thread by id.
func (c *Client) Thread(ctx context.Context, id string) (*Thread, error) {
	return get[Thread](ctx, c, indexThreads, id)
}

// ThreadTitles returns the titles of the given threads, by id.
func (c *Client) ThreadTitles(ctx context.Context, ids []string) (map[string]string, error) {
	titles := map[string]string{}
	if len(ids) == 0 {
		return titles, nil
	}
	found, err := search[Thread](ctx, c, indexThreads, map[string]any{
		"size": len(ids), "_source": []string{"id", "title"}, "query": map[string]any{"ids": map[string]any{"values": ids}},
	})
	for _, t := range found {
		titles[t.ID] = t.Title
	}
	return titles, err
}

// search runs a query and decodes each hit's source. The query is marshalled
// once, so every value in it is JSON-escaped.
func search[T any](ctx context.Context, c *Client, index string, query any) ([]T, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{Indices: []string{index}, BodyReader: bytes.NewReader(body)})
	if err != nil {
		return nil, fmt.Errorf("search %s: %w", index, err)
	}
	out := make([]T, len(resp.Hits.Hits))
	for i, h := range resp.Hits.Hits {
		if err := json.Unmarshal(h.Source, &out[i]); err != nil {
			return nil, fmt.Errorf("search %s: %w", index, err)
		}
	}
	return out, nil
}

// get fetches one document by id; ErrNotFound when there is none.
func get[T any](ctx context.Context, c *Client, index, id string) (*T, error) {
	resp, err := c.api.Document.Get(ctx, opensearchapi.GetReq{Index: index, ID: id})
	if err != nil {
		if errStatus(err) == 404 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get %s/%s: %w", index, id, err)
	}
	var out T
	if err := json.Unmarshal(resp.Source, &out); err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", index, id, err)
	}
	return &out, nil
}

// DeleteThread removes a root thread, its sub-agent threads, their messages
// and the memories made of them. Sources and artifacts stay: they are the
// evidence of what was said. The threads go last, so a failure can be retried.
func (c *Client) DeleteThread(ctx context.Context, rootID string) error {
	for _, d := range []struct{ index, field string }{
		{indexMessages, "root_id"}, {indexMemories, "thread_id"}, {indexThreads, "root_id"},
	} {
		if err := c.deleteWhere(ctx, d.index, d.field, rootID); err != nil {
			return err
		}
	}
	return nil
}

// deleteWhere deletes an index's documents whose field holds value.
func (c *Client) deleteWhere(ctx context.Context, index, field, value string) error {
	q, _ := json.Marshal(map[string]any{"query": map[string]any{"term": map[string]any{field: value}}})
	_, err := c.api.DeleteByQuery(ctx, &opensearchapi.DeleteByQueryReq{
		Indices: []string{index}, BodyReader: bytes.NewReader(q), Params: &opensearchapi.DeleteByQueryParams{Refresh: "true"},
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", index, err)
	}
	return nil
}

// SubThreads lists the ids of a conversation's sub-agent threads.
func (c *Client) SubThreads(ctx context.Context, rootID string) ([]string, error) {
	found, err := search[Thread](ctx, c, indexThreads, map[string]any{
		"size": listCap, "_source": []string{"id"}, "query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"root_id": rootID}}, map[string]any{"exists": map[string]any{"field": "parent_id"}},
		}}},
	})
	ids := make([]string, len(found))
	for i, t := range found {
		ids[i] = t.ID
	}
	return ids, err
}

// Messages returns a page of a thread's messages (see pageSize) with seq
// below before (0 means from the newest), in ascending seq order.
func (c *Client) Messages(ctx context.Context, threadID string, before int64, limit int) ([]Message, error) {
	filter := []any{map[string]any{"term": map[string]any{"thread_id": threadID}}}
	if before > 0 {
		filter = append(filter, map[string]any{"range": map[string]any{"seq": map[string]any{"lt": before}}})
	}
	out, err := search[Message](ctx, c, indexMessages, map[string]any{
		"size": pageSize(limit), "sort": []any{map[string]any{"seq": "desc"}}, "query": map[string]any{"bool": map[string]any{"filter": filter}},
	})
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}
