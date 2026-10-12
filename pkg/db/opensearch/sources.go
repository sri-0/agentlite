package opensearch

import (
	"context"
	"fmt"
	"strings"
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

// SaveSource stores a source under its conversation and number.
func (c *Client) SaveSource(ctx context.Context, s *Source) error {
	return c.index(ctx, IndexSources, sourceID(s.RootID, s.Number), s)
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

// Cite records that ref, a message or an artefact, cites the given sources
// of a conversation. Recording the same citation twice changes nothing.
func (c *Client) Cite(ctx context.Context, rootID, ref string, numbers []int) error {
	nums := make([]string, len(numbers))
	for i, n := range numbers {
		nums[i] = fmt.Sprint(n)
	}
	q := fmt.Sprintf(`{"query":{"bool":{"filter":[{"term":{"root_id":%q}},{"terms":{"number":[%s]}}]}},
		"script":{"lang":"painless","params":{"ref":%q},"source":
		"if (ctx._source.cited_in == null) { ctx._source.cited_in = []; } if (ctx._source.cited_in.contains(params.ref)) { ctx.op = 'noop'; } else { ctx._source.cited_in.add(params.ref); }"}}`,
		rootID, strings.Join(nums, ","), ref)
	_, err := c.api.UpdateByQuery(ctx, &opensearchapi.UpdateByQueryReq{
		Indices: []string{IndexSources}, BodyReader: strings.NewReader(q), Params: &opensearchapi.UpdateByQueryParams{Refresh: "true"},
	})
	if err != nil {
		return fmt.Errorf("cite sources: %w", err)
	}
	return nil
}
