// Package opensearch persists threads, messages, sources and artefacts.
// Messages are stored as coalesced AG-UI events plus the extracted text;
// nothing is converted.
package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	ErrConflict = errors.New("already exists")
)

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
		"title":{"type":"text"},"agent_id":{"type":"keyword"},"model":{"type":"keyword"},
		"connectors":{"type":"keyword"},
		"created_at":{"type":"date"},"updated_at":{"type":"date"}}}}`,
	IndexMessages: `{"mappings":{"properties":{
		"id":{"type":"keyword"},"thread_id":{"type":"keyword"},"root_id":{"type":"keyword"},"role":{"type":"keyword"},
		"seq":{"type":"long"},"created_at":{"type":"date"},"text":{"type":"text"},
		"events":{"type":"object","enabled":false}}}}`,
	IndexSources:   sourcesMapping,
	IndexArtifacts: artifactsMapping,
}

// listCap bounds the lists of one conversation's sources and artefacts.
const listCap = 1000

func (c *Client) ensureIndex(ctx context.Context, name, mapping string) error {
	_, err := c.api.Indices.Create(ctx, opensearchapi.IndicesCreateReq{Index: name, BodyReader: strings.NewReader(mapping)})
	if err != nil && errType(err) != "resource_already_exists_exception" {
		return fmt.Errorf("create index %s: %w", name, err)
	}
	return nil
}

// index upserts a document and refreshes the index at once: writes are few,
// and a thread or message must be readable as soon as its save returns.
func (c *Client) index(ctx context.Context, index, id string, doc any) error {
	return c.write(ctx, index, id, doc, opensearchapi.IndexParams{})
}

// write stores a document and refreshes the index. A write the params
// forbid (create of a taken id, an external version not above the stored
// one) keeps the stored document and returns ErrConflict.
func (c *Client) write(ctx context.Context, index, id string, doc any, params opensearchapi.IndexParams) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	params.Refresh = "true"
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
	var structured *oslib.StructError
	if errors.As(err, &structured) {
		return structured.Status
	}
	var plain *oslib.StringError
	if errors.As(err, &plain) {
		return plain.Status
	}
	return 0
}

// errType is the OpenSearch exception type of an API error, or "".
func errType(err error) string {
	var structured *oslib.StructError
	if errors.As(err, &structured) {
		return structured.Err.Type
	}
	return ""
}

// SaveThread upserts a thread.
func (c *Client) SaveThread(ctx context.Context, t *Thread) error {
	return c.index(ctx, IndexThreads, t.ID, t)
}

// SaveMessage upserts a message.
func (c *Client) SaveMessage(ctx context.Context, m *Message) error {
	return c.index(ctx, IndexMessages, m.ID, m)
}

// search runs a query and decodes each hit's source.
func search[T any](ctx context.Context, c *Client, index, query string) ([]T, error) {
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{Indices: []string{index}, BodyReader: strings.NewReader(query)})
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

// Threads lists root threads, newest first; the list is capped at threadsCap.
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	return search[Thread](ctx, c, IndexThreads, fmt.Sprintf(`{"size":%d,"sort":[{"updated_at":"desc"}],
		"query":{"bool":{"must_not":{"exists":{"field":"parent_id"}}}}}`, threadsCap))
}

const threadsCap = 200

// Thread fetches one thread by id.
func (c *Client) Thread(ctx context.Context, id string) (*Thread, error) {
	return get[Thread](ctx, c, IndexThreads, id)
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

// DeleteThread removes a root thread, its sub-agent threads and everything
// recorded for them: messages, memories, sources and artefact records.
func (c *Client) DeleteThread(ctx context.Context, id string) error {
	refresh := &opensearchapi.DeleteByQueryParams{Refresh: "true"}
	for index, field := range map[string]string{
		IndexThreads: "root_id", IndexMessages: "root_id", IndexMemories: "thread_id", IndexSources: "root_id", IndexArtifacts: "root_id",
	} {
		q := fmt.Sprintf(`{"query":{"term":{%q:%q}}}`, field, id)
		_, err := c.api.DeleteByQuery(ctx, &opensearchapi.DeleteByQueryReq{Indices: []string{index}, BodyReader: strings.NewReader(q), Params: refresh})
		if err != nil && errStatus(err) != 404 { // the memories index exists only when memory is on
			return fmt.Errorf("delete %s: %w", index, err)
		}
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
	out, err := search[Message](ctx, c, IndexMessages, q)
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}
