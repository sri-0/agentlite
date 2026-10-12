package opensearch

import (
	"bytes"
	"context"
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
	"root_id":{"type":"keyword"},"number":{"type":"integer"},"thread_id":{"type":"keyword"},"run_id":{"type":"keyword"},
	"tool":{"type":"keyword"},"tool_call_id":{"type":"keyword"},"system":{"type":"keyword"},"key":{"type":"keyword"},
	"url":{"type":"keyword"},"title":{"type":"text"},"metadata":{"type":"flat_object"},
	"retrieved_at":{"type":"date"},"cited_in":{"type":"keyword"}}}}`

func sourceID(rootID string, number int) string { return fmt.Sprintf("%s:%d", rootID, number) }

// SaveSource stores a new source. A number is never reused: when the
// conversation already has a source with this number, it is kept and
// ErrConflict returned.
func (c *Client) SaveSource(ctx context.Context, s *Source) error {
	return c.write(ctx, IndexSources, sourceID(s.RootID, s.Number), s, opensearchapi.IndexParams{OpType: opensearchapi.OpTypeCreate})
}

// Sources lists a conversation's sources by number.
func (c *Client) Sources(ctx context.Context, rootID string) ([]Source, error) {
	return search[Source](ctx, c, IndexSources, fmt.Sprintf(`{"size":%d,"sort":[{"number":"asc"}],
		"query":{"term":{"root_id":%q}}}`, listCap, rootID))
}

// Source fetches one source of a conversation by number.
func (c *Client) Source(ctx context.Context, rootID string, number int) (*Source, error) {
	return get[Source](ctx, c, IndexSources, sourceID(rootID, number))
}

// LastSourceNumber is the highest number a conversation's sources have, or 0.
func (c *Client) LastSourceNumber(ctx context.Context, rootID string) (int, error) {
	last, err := search[Source](ctx, c, IndexSources, fmt.Sprintf(`{"size":1,"sort":[{"number":"desc"}],
		"_source":["number"],"query":{"term":{"root_id":%q}}}`, rootID))
	if err != nil || len(last) == 0 {
		return 0, err
	}
	return last[0].Number, nil
}

const citeScript = "if (ctx._source.cited_in == null) { ctx._source.cited_in = []; } " +
	"if (ctx._source.cited_in.contains(params.ref)) { ctx.op = 'noop'; } else { ctx._source.cited_in.add(params.ref); }"

// Cite records that ref, a message or an artefact, cites the given sources
// of a conversation. Each source is updated on its own and retried when
// another citation is written at the same moment; a number with no source
// (one the model made up) is skipped.
func (c *Client) Cite(ctx context.Context, rootID, ref string, numbers []int) error {
	if len(numbers) == 0 {
		return nil
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, n := range numbers {
		action := map[string]any{"update": map[string]any{"_index": IndexSources, "_id": sourceID(rootID, n), "retry_on_conflict": 3}}
		script := map[string]any{"script": map[string]any{"lang": "painless", "source": citeScript, "params": map[string]any{"ref": ref}}}
		if err := errors.Join(enc.Encode(action), enc.Encode(script)); err != nil {
			return err
		}
	}
	resp, err := c.api.Bulk(ctx, opensearchapi.BulkReq{Body: &body, Params: &opensearchapi.BulkParams{Refresh: "true"}})
	if err != nil {
		return fmt.Errorf("cite sources: %w", err)
	}
	var errs []error
	for i, item := range resp.Items { // in request order
		if u := item.Update; u != nil && u.Error != nil && u.Status != 404 {
			errs = append(errs, fmt.Errorf("cite source %d: %s", numbers[i], u.Error.Type))
		}
	}
	return errors.Join(errs...)
}
