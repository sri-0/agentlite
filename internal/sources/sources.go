// Package sources numbers and stores the sources tools return, so answers can
// cite them as [source:N] and every citation traces back to the record a
// tool made. Numbers belong to a conversation (the root thread), so a
// sub-agent's citations stay valid in its parent's answer. Everything lives
// in OpenSearch, under the same access rules as the conversation.
package sources

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agentlite/pkg/citations"
	"agentlite/pkg/db/opensearch"
)

// Recorder implements citations.Recorder on OpenSearch.
type Recorder struct {
	store *opensearch.Client
}

// New returns a recorder.
func New(store *opensearch.Client) *Recorder {
	return &Recorder{store: store}
}

// Add numbers a source and stores its record. An item recorded before in the
// conversation (the same system and key, or URL) keeps its number. Two
// records of one new item at the same moment collide on its id; the loser
// takes the winner's number, and its own is left unused.
func (r *Recorder) Add(ctx context.Context, from citations.Origin, s citations.Source) (int, error) {
	identity := s.Key
	if identity == "" {
		identity = s.URL
	}
	keyed := identity != ""
	var id string
	if keyed {
		id = opensearch.SourceKeyID(from.RootID, s.System+"\x00"+identity)
		if n, err := r.numberOf(ctx, id); !errors.Is(err, opensearch.ErrNotFound) {
			return n, err
		}
	}
	n, err := r.store.NextSourceNumber(ctx, from.RootID)
	if err != nil {
		return 0, err
	}
	if !keyed {
		id = opensearch.SourceNumberID(from.RootID, n)
	}
	err = r.store.SaveSource(ctx, &opensearch.Source{
		ID: id, RootID: from.RootID, Number: n,
		ThreadID: from.ThreadID, RunID: from.RunID, Tool: from.Tool, ToolCallID: from.CallID,
		System: s.System, Key: s.Key, URL: s.URL, Title: s.Title, Metadata: s.Metadata, RetrievedAt: time.Now(),
	})
	switch {
	case errors.Is(err, opensearch.ErrConflict) && keyed:
		return r.numberOf(ctx, id)
	case err != nil:
		return 0, fmt.Errorf("save source %d: %w", n, err)
	}
	return n, nil
}

func (r *Recorder) numberOf(ctx context.Context, id string) (int, error) {
	s, err := r.store.SourceByID(ctx, id)
	if err != nil {
		return 0, err
	}
	return s.Number, nil
}

// Cite records that ref ("message:<id>" or "artifact:<id>") cites the
// sources a text names as [source:N].
func (r *Recorder) Cite(ctx context.Context, rootID, ref, text string) error {
	return r.store.Cite(ctx, rootID, ref, citations.Numbers(text))
}
