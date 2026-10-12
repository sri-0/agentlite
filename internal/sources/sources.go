// Package sources numbers and stores the sources tools return, so answers can
// cite them as [source:N] and every citation traces back to the record a
// tool made. Numbers belong to a conversation (the root thread), so a
// sub-agent's citations stay valid in its parent's answer.
package sources

import (
	"context"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"

	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/toolx"
)

// Recorder implements toolx.Sources: numbers in Valkey, records in OpenSearch.
type Recorder struct {
	db    valkey.Client
	store *opensearch.Client
}

// New returns a recorder.
func New(db valkey.Client, store *opensearch.Client) *Recorder {
	return &Recorder{db: db, store: store}
}

func keysKey(rootID string) string { return "thread:" + rootID + ":sources" }
func seqKey(rootID string) string  { return "thread:" + rootID + ":sources:seq" }

// numberScript gives an item its number in a conversation: the one its key
// already has, or the next one. An item without a key always gets the next.
// It returns the number and 1 when the number is new.
var numberScript = valkey.NewLuaScript(`
if ARGV[1] ~= '' then
  local n = redis.call('HGET', KEYS[1], ARGV[1])
  if n then return {tonumber(n), 0} end
end
local n = redis.call('INCR', KEYS[2])
if ARGV[1] ~= '' then redis.call('HSET', KEYS[1], ARGV[1], n) end
return {n, 1}`)

// Add numbers the source and, the first time its key is seen in the
// conversation, stores its record.
func (r *Recorder) Add(ctx context.Context, from toolx.Call, s toolx.Source) (int, error) {
	key := s.Key
	if key == "" {
		key = s.URL
	}
	if key != "" {
		key = s.System + " " + key
	}
	res, err := numberScript.Exec(ctx, r.db, []string{keysKey(from.Thread.RootID), seqKey(from.Thread.RootID)}, []string{key}).ToArray()
	if err != nil || len(res) != 2 {
		return 0, fmt.Errorf("number source: %w", err)
	}
	n, _ := res[0].AsInt64()
	if fresh, _ := res[1].AsInt64(); fresh == 0 {
		return int(n), nil
	}
	err = r.store.SaveSource(ctx, &opensearch.Source{
		RootID: from.Thread.RootID, Number: int(n), ThreadID: from.Thread.ID, RunID: from.Thread.RunID, Tool: from.Tool, ToolCallID: from.ID,
		System: s.System, Key: s.Key, URL: s.URL, Title: s.Title, Metadata: s.Metadata, RetrievedAt: time.Now(),
	})
	if err != nil && key != "" {
		// Forget the key so the next call stores the record under a new number.
		r.db.Do(context.WithoutCancel(ctx), r.db.B().Hdel().Key(keysKey(from.Thread.RootID)).Field(key).Build())
	}
	return int(n), err
}

// Cite records that ref ("message:<id>") cites the sources a text names as
// [source:N].
func (r *Recorder) Cite(ctx context.Context, rootID, ref, text string) error {
	cited := toolx.Citations(text)
	if len(cited) == 0 {
		return nil
	}
	return r.store.Cite(ctx, rootID, ref, cited)
}

// Delete forgets a conversation's numbering; its records are deleted with
// the thread's documents.
func (r *Recorder) Delete(ctx context.Context, rootID string) error {
	return r.db.Do(ctx, r.db.B().Del().Key(keysKey(rootID), seqKey(rootID)).Build()).Error()
}
