package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey keys for run records.
const (
	runTTL   = 7 * 24 * time.Hour
	leaseTTL = 60 * time.Second

	activeSetKey = "runs:active"
)

func runKey(id string) string           { return "run:" + id }
func threadRunKey(thread string) string { return "thread:" + thread + ":run" }
func threadSeqKey(thread string) string { return "thread:" + thread + ":seq" }
func ctlChannel(runID string) string    { return "run:" + runID + ":ctl" }
func doneChannel(runID string) string   { return "run:" + runID + ":done" }

// runs stores run records and the per-thread active pointer in Valkey.
type runs struct{ db valkey.Client }

func (r *runs) save(ctx context.Context, run *Run) error {
	return r.update(ctx, run.ID, run.fields())
}

func (r *runs) update(ctx context.Context, id string, fields map[string]string) error {
	fields["updated_at"] = fmt.Sprint(time.Now().UnixMilli())
	cmd := r.db.B().Hset().Key(runKey(id)).FieldValue()
	for k, v := range fields {
		cmd = cmd.FieldValue(k, v)
	}
	for _, res := range r.db.DoMulti(ctx, cmd.Build(), r.db.B().Expire().Key(runKey(id)).Seconds(int64(runTTL.Seconds())).Build()) {
		if err := res.Error(); err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	return nil
}

func (r *runs) get(ctx context.Context, id string) (*Run, error) {
	f, err := r.db.Do(ctx, r.db.B().Hgetall().Key(runKey(id)).Build()).AsStrMap()
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	if len(f) == 0 {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	return runFromFields(f), nil
}

// claimThread marks the run as the thread's active run; false if one exists.
func (r *runs) claimThread(ctx context.Context, threadID, runID string) (bool, error) {
	res := r.db.Do(ctx, r.db.B().Set().Key(threadRunKey(threadID)).Value(runID).Nx().Ex(leaseTTL).Build())
	if err := res.Error(); err != nil {
		if valkey.IsValkeyNil(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *runs) refreshLease(ctx context.Context, threadID string) error {
	return r.db.Do(ctx, r.db.B().Expire().Key(threadRunKey(threadID)).Seconds(int64(leaseTTL.Seconds())).Build()).Error()
}

// persistLease keeps the pointer without expiry while a run waits for input.
func (r *runs) persistLease(ctx context.Context, threadID string) error {
	return r.db.Do(ctx, r.db.B().Persist().Key(threadRunKey(threadID)).Build()).Error()
}

// releaseScript deletes the thread's active pointer only while it still
// names the run, so a redelivered notice cannot release a newer run.
var releaseScript = valkey.NewLuaScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)

func (r *runs) releaseThread(ctx context.Context, threadID, runID string) error {
	return releaseScript.Exec(ctx, r.db, []string{threadRunKey(threadID)}, []string{runID}).Error()
}

// active returns the thread's active run, or nil.
func (r *runs) active(ctx context.Context, threadID string) (*Run, error) {
	id, err := r.db.Do(ctx, r.db.B().Get().Key(threadRunKey(threadID)).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, err
	}
	run, err := r.get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return run, err
}

func (r *runs) nextSeq(ctx context.Context, threadID string) (int64, error) {
	return r.db.Do(ctx, r.db.B().Incr().Key(threadSeqKey(threadID)).Build()).AsInt64()
}

func (r *runs) setActive(ctx context.Context, runID string, active bool) error {
	if active {
		return r.db.Do(ctx, r.db.B().Sadd().Key(activeSetKey).Member(runID).Build()).Error()
	}
	return r.db.Do(ctx, r.db.B().Srem().Key(activeSetKey).Member(runID).Build()).Error()
}

func (r *runs) activeIDs(ctx context.Context) ([]string, error) {
	return r.db.Do(ctx, r.db.B().Smembers().Key(activeSetKey).Build()).AsStrSlice()
}

func (r *runs) publish(ctx context.Context, channel, msg string) {
	r.db.Do(ctx, r.db.B().Publish().Channel(channel).Message(msg).Build())
}
