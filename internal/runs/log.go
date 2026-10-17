package runs

import (
	"context"
	"fmt"
	"iter"
	"strconv"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/stream"
)

// Entry is one event in a thread's log with its cursor and raw JSON.
type Entry struct {
	Seq   string
	Raw   []byte
	Event stream.Event
}

const (
	logRetention = 24 * time.Hour
	logExpiry    = 7 * 24 * time.Hour
	readBlock    = 5 * time.Second
	readBatch    = 256
)

// valkeyLog is the per-thread event stream, a Valkey stream: the live channel
// between a running agent and its subscribers, and the source the finalizer
// folds from.
type valkeyLog struct{ db valkey.Client }

func newValkeyLog(db valkey.Client) *valkeyLog { return &valkeyLog{db: db} }

func logKey(threadID string) string { return "thread:" + threadID + ":log" }

// Append adds an event produced by a run and returns its cursor.
func (l *valkeyLog) Append(ctx context.Context, threadID string, e stream.Event) (string, error) {
	raw, err := e.ToJSON()
	if err != nil {
		return "", err
	}
	key := logKey(threadID)
	res := l.db.DoMulti(ctx,
		l.db.B().Xadd().Key(key).Id("*").FieldValue().FieldValue("e", string(raw)).Build(),
		l.db.B().Expire().Key(key).Seconds(int64(logExpiry.Seconds())).Build())
	seq, err := res[0].ToString()
	if err != nil {
		return "", fmt.Errorf("log append: %w", err)
	}
	return seq, nil
}

// Last returns the newest cursor, or "0" for an empty log.
func (l *valkeyLog) Last(ctx context.Context, threadID string) (string, error) {
	entries, err := l.db.Do(ctx, l.db.B().Xrevrange().Key(logKey(threadID)).End("+").Start("-").Count(1).Build()).AsXRange()
	if err != nil {
		return "", fmt.Errorf("log last: %w", err)
	}
	if len(entries) == 0 {
		return "0", nil
	}
	return entries[0].ID, nil
}

// Range returns every event after the cursor without blocking.
func (l *valkeyLog) Range(ctx context.Context, threadID, after string) ([]Entry, error) {
	start := "-"
	if after != "" && after != "0" {
		start = "(" + after
	}
	raw, err := l.db.Do(ctx, l.db.B().Xrange().Key(logKey(threadID)).Start(start).End("+").Build()).AsXRange()
	if err != nil {
		return nil, fmt.Errorf("log range: %w", err)
	}
	return parseEntries(raw)
}

// Read yields events after the cursor, blocking for new ones until ctx ends.
// The caller stops when it has what it needs.
func (l *valkeyLog) Read(ctx context.Context, threadID, after string) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		cursor := after
		if cursor == "" {
			cursor = "0"
		}
		for ctx.Err() == nil {
			res, err := l.db.Do(ctx, l.db.B().Xread().Count(readBatch).Block(readBlock.Milliseconds()).
				Streams().Key(logKey(threadID)).Id(cursor).Build()).AsXRead()
			if err != nil {
				if valkey.IsValkeyNil(err) {
					continue // block timed out; poll again
				}
				if ctx.Err() == nil {
					yield(Entry{}, fmt.Errorf("log read: %w", err))
				}
				return
			}
			entries, err := parseEntries(res[logKey(threadID)])
			if err != nil {
				yield(Entry{}, err)
				return
			}
			for _, e := range entries {
				cursor = e.Seq
				if !yield(e, nil) {
					return
				}
			}
		}
	}
}

// Trim drops events older than the retention.
func (l *valkeyLog) Trim(ctx context.Context, threadID string) error {
	minID := strconv.FormatInt(time.Now().Add(-logRetention).UnixMilli(), 10)
	return l.db.Do(ctx, l.db.B().Xtrim().Key(logKey(threadID)).Minid().Threshold(minID).Build()).Error()
}

func parseEntries(raw []valkey.XRangeEntry) ([]Entry, error) {
	out := make([]Entry, 0, len(raw))
	for _, x := range raw {
		ev, err := events.EventFromJSON([]byte(x.FieldValues["e"]))
		if err != nil {
			return nil, fmt.Errorf("log entry %s: %w", x.ID, err)
		}
		out = append(out, Entry{Seq: x.ID, Raw: []byte(x.FieldValues["e"]), Event: ev})
	}
	return out, nil
}
