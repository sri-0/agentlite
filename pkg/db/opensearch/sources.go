package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v5/opensearchapi"
)

// indexSources holds the sources tools returned, numbered per conversation.
const indexSources = "sources"

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

// SourceKeyID is the document id of a source with an identity (its system
// and key or URL): a name-based UUID, one per conversation, so recording it
// twice collides instead of numbering it twice.
func SourceKeyID(rootID, identity string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(rootID+"\x00"+identity)).String()
}

// SaveSource stores a new source, at once searchable for the citations that
// follow. A record is never replaced: when one with the same id exists, it is
// kept and ErrConflict returned.
func (c *Client) SaveSource(ctx context.Context, s *Source) error {
	params := opensearchapi.IndexParams{OpType: opensearchapi.OpTypeCreate, Refresh: "true"}
	return c.write(ctx, indexSources, s.ID, s, params)
}

// SourceByID fetches one source by document id.
func (c *Client) SourceByID(ctx context.Context, id string) (*Source, error) {
	return get[Source](ctx, c, indexSources, id)
}

// Sources lists a conversation's sources by number.
func (c *Client) Sources(ctx context.Context, rootID string) ([]Source, error) {
	return search[Source](ctx, c, indexSources, map[string]any{
		"size": listCap, "sort": []any{map[string]any{"number": "asc"}}, "query": map[string]any{"term": map[string]any{"root_id": rootID}},
	})
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
	return search[Source](ctx, c, indexSources, map[string]any{
		"size": len(numbers), "query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"root_id": rootID}}, map[string]any{"terms": map[string]any{"number": numbers}},
		}}},
	})
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
			"update": map[string]any{"_index": indexSources, "_id": s.ID, "retry_on_conflict": 3},
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
