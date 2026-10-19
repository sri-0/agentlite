// Package opensearch persists threads, messages, sources and artifacts.
// Messages are stored as coalesced AG-UI events plus the extracted text;
// nothing is converted. The indices and their mappings are created outside
// the service, from deploy/opensearch (make indices). Every call is made for
// the Caller in its context, with their token (access.go): the server has no
// OpenSearch credentials of its own.
package opensearch

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"slices"
	"time"

	oslib "github.com/opensearch-project/opensearch-go/v5"
	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// Config is the OpenSearch connection. IndexPrefix starts every index's
// name (user_ai_threads, user_ai_userfiles, …), so several deployments can
// share a cluster; set it empty for bare names. There are no credentials:
// each call carries its caller's token.
type Config struct {
	URL         string `env:"URL, default=http://localhost:9200"`
	IndexPrefix string `env:"INDEX_PREFIX, default=user_ai_"`
	// The file chunks' and the memories' indexes, without the prefix, from
	// models.yaml (embedding.index, memory.index) rather than the
	// environment; "" is indexFileChunks or indexMemories.
	ChunkIndex, MemoryIndex string
}

// Index names, without the deployment's prefix (Client.ix adds it).
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
	Connectors  *[]string `json:"connectors,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"` // the workspace a root thread is filed under
	Pinned      bool      `json:"pinned,omitempty"`       // kept at the top of the sidebar
	Archived    bool      `json:"archived,omitempty"`     // left out of the sidebar
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Labels
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
	Labels
}

// Errors the store returns for a missing document, and for creating one
// whose id is taken.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("write conflict")
)

// Client is the OpenSearch store.
type Client struct {
	api              *opensearchapi.Client
	prefix           string
	chunks, memories string // the two indexes models.yaml can name
}

// New returns a client of the cluster. Nothing it sends goes without a
// caller's token: the background checks that would (discovery, health
// checks) are off, and a URL with credentials is refused.
func New(cfg Config) (*Client, error) {
	if u, err := url.Parse(cfg.URL); err != nil || u.User != nil {
		return nil, errors.New("opensearch: OPENSEARCH_URL must be a URL without credentials")
	}
	api, err := opensearchapi.NewClient(opensearchapi.Config{Client: oslib.Config{
		Addresses: []string{cfg.URL}, Signer: bearer{},
		DiscoverNodesOnStart:  new(bool), // false: the configured URL is the only reachable address
		HealthCheckMaxRetries: -1, MaxRetryClusterHealth: -1,
	}})
	if err != nil {
		return nil, fmt.Errorf("opensearch: %w", err)
	}
	return &Client{api: api, prefix: cfg.IndexPrefix,
		chunks: cmp.Or(cfg.ChunkIndex, indexFileChunks), memories: cmp.Or(cfg.MemoryIndex, indexMemories)}, nil
}

// pageSize is how many entries a page of a list holds: 50 unless the client
// asks for another number, and at most 200.
func pageSize(limit int) int {
	if limit <= 0 {
		return 50
	}
	return min(limit, 200)
}

// listCap bounds the lists of one conversation's sources and artifacts, the
// skills library and the workspaces.
const listCap = 1000

// inRoots is a query filter on the conversations in roots, or none when roots
// is nil; an empty roots matches nothing.
func inRoots(roots []string) []any {
	if roots == nil {
		return []any{}
	}
	return []any{map[string]any{"terms": map[string]any{"root_id": roots}}}
}

// ix is an index's full name: the deployment's prefix, then its own.
func (c *Client) ix(index string) string { return c.prefix + index }

// delete removes a document its caller created and refreshes the index;
// ErrNotFound when there is none, ErrForbidden when someone else created it.
// The document is read first, as the caller: OpenSearch does not apply
// document-level security to a delete by id, so one the caller cannot see
// would go.
func (c *Client) delete(ctx context.Context, index, id string) error {
	l, err := get[Labels](ctx, c, index, id)
	if err != nil {
		return err
	}
	if err := CheckOwner(ctx, *l); err != nil {
		return err
	}
	_, err = c.api.Document.Delete(ctx, opensearchapi.DeleteReq{
		Index: c.ix(index), ID: id, Params: &opensearchapi.DeleteParams{Refresh: "true"},
	})
	switch {
	case errStatus(err) == 404:
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("delete %s/%s: %w", index, id, err)
	}
	return nil
}

// refreshed makes a write readable by searches as soon as it returns.
var refreshed = opensearchapi.IndexParams{Refresh: "true"}

// index upserts a document and refreshes the index at once: writes are few,
// and a thread or message must be readable as soon as its save returns
// (under document-level security a get is not realtime either).
func (c *Client) index(ctx context.Context, index, id string, doc labelled) error {
	return c.write(ctx, index, id, doc, refreshed)
}

// write stamps and stores a document (see stamp). A write the params forbid
// (create of a taken id, an external version not above the stored one, a
// document changed since it was read) keeps the stored document and returns
// ErrConflict.
func (c *Client) write(ctx context.Context, index, id string, doc labelled, params opensearchapi.IndexParams) error {
	if err := stamp(ctx, doc.labels()); err != nil {
		return fmt.Errorf("index %s/%s: %w", index, id, err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = c.api.Document.Index(ctx, opensearchapi.IndexReq{Index: c.ix(index), ID: id, Body: bytes.NewReader(body), Params: &params})
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

// SaveMessage upserts a message the server named.
func (c *Client) SaveMessage(ctx context.Context, m *Message) error {
	return c.index(ctx, indexMessages, m.ID, m)
}

// CreateMessage stores a new message whose id a client chose; ErrConflict
// when the id is taken. It never replaces one: an index by id would replace
// a message of any control set.
func (c *Client) CreateMessage(ctx context.Context, m *Message) error {
	return c.write(ctx, indexMessages, m.ID, m, opensearchapi.IndexParams{OpType: opensearchapi.OpTypeCreate, Refresh: "true"})
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

// search runs a query, confined to the caller's control sets, and decodes
// each hit's source. The query is marshalled once, so every value in it is
// JSON-escaped. A caller with no control set sees nothing.
func search[T any](ctx context.Context, c *Client, index string, query map[string]any) ([]T, error) {
	caller, ok := CallerFrom(ctx)
	if !ok {
		return nil, ErrNoCaller
	}
	if len(caller.ControlSets) == 0 {
		return nil, nil
	}
	confine(query, caller.ControlSets)
	body, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	resp, err := c.api.Search(ctx, &opensearchapi.SearchReq{Indices: []string{c.ix(index)}, BodyReader: bytes.NewReader(body)})
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

// get fetches one document by id; ErrNotFound when there is none, or when
// it is outside the caller's control sets.
func get[T any](ctx context.Context, c *Client, index, id string) (*T, error) {
	doc, _, err := getAt[T](ctx, c, index, id)
	return doc, err
}

// at is where a document stands in its shard's history: a write conditioned
// on it fails with ErrConflict if the document changed since it was read.
type at struct{ seq, term int }

// getAt is get, with where the document stands.
func getAt[T any](ctx context.Context, c *Client, index, id string) (*T, at, error) {
	caller, ok := CallerFrom(ctx)
	if !ok {
		return nil, at{}, ErrNoCaller
	}
	resp, err := c.api.Document.Get(ctx, opensearchapi.GetReq{Index: c.ix(index), ID: id})
	if err != nil {
		if errStatus(err) == 404 {
			return nil, at{}, ErrNotFound
		}
		return nil, at{}, fmt.Errorf("get %s/%s: %w", index, id, err)
	}
	var out T
	if err := json.Unmarshal(resp.Source, &out); err != nil {
		return nil, at{}, fmt.Errorf("get %s/%s: %w", index, id, err)
	}
	l, ok := any(&out).(labelled)
	if !ok || !slices.Contains(caller.ControlSets, l.labels().ControlSetID) {
		return nil, at{}, ErrNotFound
	}
	var pos at
	if resp.SeqNo != nil && resp.PrimaryTerm != nil {
		pos = at{seq: int(*resp.SeqNo), term: int(*resp.PrimaryTerm)}
	}
	return &out, pos, nil
}

// writeTries bounds how often update reads and writes a document that keeps
// changing under it: twenty writers at once all land.
const writeTries = 12

// update changes a document in place: it reads it, lets edit change it, and
// writes it back on the condition that nobody wrote it in between, reading
// again when someone did. OpenSearch refuses _update (scripted or partial)
// under document-level security, so this is how a document changes. A
// missing document is created from the zero value when create is set, and is
// ErrNotFound otherwise; edit returning false leaves it as it is.
func update[T any, P interface {
	*T
	labelled
}](ctx context.Context, c *Client, index, id string, create bool, edit func(P) bool) (P, error) {
	for try := range writeTries {
		if try > 0 { // a random while, so writers that collided do not collide again
			time.Sleep(rand.N(min(10*time.Millisecond<<try, time.Second)))
		}
		found, pos, err := getAt[T](ctx, c, index, id)
		doc, params := P(found), opensearchapi.IndexParams{Refresh: "true", IfSeqNo: &pos.seq, IfPrimaryTerm: &pos.term}
		switch {
		case errors.Is(err, ErrNotFound) && create:
			doc, params = P(new(T)), opensearchapi.IndexParams{Refresh: "true", OpType: opensearchapi.OpTypeCreate}
		case err != nil:
			return nil, err
		}
		if !edit(doc) {
			return doc, nil
		}
		if err := c.write(ctx, index, id, doc, params); !errors.Is(err, ErrConflict) {
			return doc, err
		}
	}
	return nil, fmt.Errorf("update %s/%s: %w %d times", index, id, ErrConflict, writeTries)
}

// DeleteThread removes a root thread, its sub-agent threads, their messages
// and the memories made of them. Sources and artifacts stay: they are the
// evidence of what was said. The threads go last, so a failure can be retried.
// The caller checks the thread is theirs (CheckOwner) first.
func (c *Client) DeleteThread(ctx context.Context, rootID string) error {
	for _, d := range []struct{ index, field string }{
		{indexMessages, "root_id"}, {c.memories, "thread_id"}, {indexThreads, "root_id"},
	} {
		if err := c.deleteWhere(ctx, d.index, d.field, rootID); err != nil {
			return err
		}
	}
	return nil
}

// deleteWhere deletes an index's documents whose field holds value, of the
// caller's control sets.
func (c *Client) deleteWhere(ctx context.Context, index, field, value string) error {
	caller, ok := CallerFrom(ctx)
	if !ok {
		return ErrNoCaller
	}
	body := map[string]any{"query": map[string]any{"term": map[string]any{field: value}}}
	confine(body, caller.ControlSets)
	q, _ := json.Marshal(body)
	_, err := c.api.DeleteByQuery(ctx, &opensearchapi.DeleteByQueryReq{
		Indices: []string{c.ix(index)}, BodyReader: bytes.NewReader(q), Params: &opensearchapi.DeleteByQueryParams{Refresh: "true"},
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
