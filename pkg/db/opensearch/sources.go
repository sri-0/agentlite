package opensearch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// IndexSources holds the sources tools returned, numbered per conversation.
const IndexSources = "sources"

// Source is an item a tool returned that answers may cite as [source:N]: the
// record the tool made of it, and where it has been cited since.
type Source struct {
	ID          string         `json:"id"`      // the document id (see SourceID)
	RootID      string         `json:"root_id"` // the conversation the number belongs to
	Number      int            `json:"number"`
	ThreadID    string         `json:"thread_id"` // where the tool was called
	RunID       string         `json:"run_id"`
	Tool        string         `json:"tool"`
	ToolCallID  string         `json:"tool_call_id"`
	System      string         `json:"system"`
	Key         string         `json:"key,omitempty"`
	URL         string         `json:"url,omitempty"`
	Title       string         `json:"title,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	RetrievedAt time.Time      `json:"retrieved_at"`
	// CitedIn names what cites it: "message:<id>" or "artifact:<id>".
	CitedIn []string `json:"cited_in,omitempty"`
}

const sourcesMapping = `{"mappings":{"properties":{
	"id":{"type":"keyword"},"root_id":{"type":"keyword"},"number":{"type":"integer"},
	"thread_id":{"type":"keyword"},"run_id":{"type":"keyword"},"tool":{"type":"keyword"},"tool_call_id":{"type":"keyword"},
	"system":{"type":"keyword"},"key":{"type":"keyword"},"url":{"type":"keyword"},"title":{"type":"text"},
	"metadata":{"type":"flat_object"},"retrieved_at":{"type":"date"},"cited_in":{"type":"keyword"}}}}`

// SourceKeyID is the document id of a source with an identity (its system
// and key or URL): one per conversation, so recording it twice collides
// instead of numbering it twice.
func SourceKeyID(rootID, identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return rootID + ":k:" + hex.EncodeToString(sum[:16])
}

// SourceNumberID is the document id of a source without an identity.
func SourceNumberID(rootID string, number int) string {
	return fmt.Sprintf("%s:n:%d", rootID, number)
}

// SaveSource stores a new source. A record is never replaced: when one with
// the same id exists, it is kept and ErrConflict returned.
func (c *Client) SaveSource(ctx context.Context, s *Source) error {
	return c.write(ctx, IndexSources, s.ID, s, opensearchapi.IndexParams{OpType: opensearchapi.OpTypeCreate})
}

// SourceByID fetches one source by document id.
func (c *Client) SourceByID(ctx context.Context, id string) (*Source, error) {
	return get[Source](ctx, c, IndexSources, id)
}

// Sources lists a conversation's sources by number.
func (c *Client) Sources(ctx context.Context, rootID string) ([]Source, error) {
	return search[Source](ctx, c, IndexSources, fmt.Sprintf(`{"size":%d,"sort":[{"number":"asc"}],
		"query":{"term":{"root_id":%q}}}`, listCap, rootID))
}

// Source fetches one source of a conversation by number.
func (c *Client) Source(ctx context.Context, rootID string, number int) (*Source, error) {
	found, err := c.sourcesNumbered(ctx, rootID, []int{number})
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

func (c *Client) sourcesNumbered(ctx context.Context, rootID string, numbers []int) ([]Source, error) {
	q, err := json.Marshal(map[string]any{
		"size": len(numbers),
		"query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"root_id": rootID}},
			map[string]any{"terms": map[string]any{"number": numbers}},
		}}},
	})
	if err != nil {
		return nil, err
	}
	return search[Source](ctx, c, IndexSources, string(q))
}

const citeScript = "if (ctx._source.cited_in == null) { ctx._source.cited_in = []; } " +
	"if (ctx._source.cited_in.contains(params.ref)) { ctx.op = 'noop'; } else { ctx._source.cited_in.add(params.ref); }"

// Cite records that ref, a message or an artifact, cites the given sources
// of a conversation. Each source is updated on its own and retried when
// another citation is written at the same moment; a number with no source
// (one the model made up) is skipped.
func (c *Client) Cite(ctx context.Context, rootID, ref string, numbers []int) error {
	if len(numbers) == 0 {
		return nil
	}
	cited, err := c.sourcesNumbered(ctx, rootID, numbers)
	if err != nil || len(cited) == 0 {
		return err
	}
	script, err := json.Marshal(map[string]any{
		"script": map[string]any{"lang": "painless", "source": citeScript, "params": map[string]any{"ref": ref}},
	})
	if err != nil {
		return err
	}
	var body bytes.Buffer
	for _, s := range cited {
		action, _ := json.Marshal(map[string]any{
			"update": map[string]any{"_index": IndexSources, "_id": s.ID, "retry_on_conflict": 3},
		})
		body.Write(action)
		body.WriteByte('\n')
		body.Write(script)
		body.WriteByte('\n')
	}
	// A failed item comes back as a PartialBulkError with the response; the
	// items are checked below.
	resp, err := c.api.Bulk(ctx, opensearchapi.BulkReq{Body: &body, Params: &opensearchapi.BulkParams{Refresh: "true"}})
	if err != nil && !errors.As(err, new(*opensearchapi.PartialBulkError)) {
		return fmt.Errorf("cite sources: %w", err)
	}
	var errs []error
	for i, item := range resp.Items { // in request order
		if u := item.Update; u != nil && u.Error != nil {
			errs = append(errs, fmt.Errorf("cite source %d: %s", cited[i].Number, u.Error.Type))
		}
	}
	return errors.Join(errs...)
}
