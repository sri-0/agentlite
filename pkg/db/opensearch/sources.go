package opensearch

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	Marks
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

// SourceKey is a source's place in RecentSources' order: newest first, ties
// broken by id, so a page never skips one.
type SourceKey struct {
	RetrievedAt int64  `json:"t"` // Unix milliseconds, as the index stores it
	ID          string `json:"i"`
}

// RecentSources lists a page of sources across every conversation, or only
// those in roots when it isn't nil (see pageSize), only the ones cited when
// cited is set, after the given one (nil: from the newest). next is where the
// following page starts; nil when this one was the last.
func (c *Client) RecentSources(ctx context.Context, roots []string, cited bool, after *SourceKey, limit int) (found []Source, next *SourceKey, err error) {
	size := pageSize(limit)
	filter := inRoots(roots)
	if cited {
		filter = append(filter, map[string]any{"exists": map[string]any{"field": "cited_in"}})
	}
	q := map[string]any{
		"size":  size,
		"sort":  []any{map[string]any{"retrieved_at": "desc"}, map[string]any{"id": "asc"}},
		"query": map[string]any{"bool": map[string]any{"filter": filter}},
	}
	if after != nil {
		q["search_after"] = []any{after.RetrievedAt, after.ID}
	}
	found, err = search[Source](ctx, c, indexSources, q)
	if err != nil || len(found) < size {
		return found, nil, err
	}
	last := found[len(found)-1]
	return found, &SourceKey{RetrievedAt: last.RetrievedAt.UnixMilli(), ID: last.ID}, nil
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

// Cite records that ref, a message or an artifact, cites the given sources
// of a conversation. Each source is changed on its own (update), again when
// another citation is written at the same moment; a number with no source
// (one the model made up) is skipped.
func (c *Client) Cite(ctx context.Context, rootID, ref string, numbers []int) error {
	if len(numbers) == 0 {
		return nil
	}
	cited, err := c.sourcesNumbered(ctx, rootID, numbers)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range cited {
		_, err := update(ctx, c, indexSources, s.ID, false, func(s *Source) bool {
			if slices.Contains(s.CitedIn, ref) {
				return false
			}
			s.CitedIn = append(s.CitedIn, ref)
			return true
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("cite source %d: %w", s.Number, err))
		}
	}
	return errors.Join(errs...)
}
