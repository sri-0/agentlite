package runs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey keys and lifetimes for run records.
const (
	runTTL   = 7 * 24 * time.Hour
	leaseTTL = 60 * time.Second
)

func runKey(id string) string           { return "run:" + id }
func threadRunKey(thread string) string { return "thread:" + thread + ":run" }
func threadSeqKey(thread string) string { return "thread:" + thread + ":seq" }
func ctlChannel(runID string) string    { return "run:" + runID + ":ctl" }

// store keeps run records and the per-thread active pointer in Valkey.
type store struct{ db valkey.Client }

func (s *store) save(ctx context.Context, run *Run) error {
	return s.update(ctx, run.ID, run.fields())
}

func (s *store) update(ctx context.Context, id string, fields map[string]string) error {
	fields["updated_at"] = fmt.Sprint(time.Now().UnixMilli())
	cmd := s.db.B().Hset().Key(runKey(id)).FieldValue()
	for k, v := range fields {
		cmd = cmd.FieldValue(k, v)
	}
	for _, res := range s.db.DoMulti(ctx, cmd.Build(), s.db.B().Expire().Key(runKey(id)).Seconds(int64(runTTL.Seconds())).Build()) {
		if err := res.Error(); err != nil {
			return fmt.Errorf("run %s: %w", id, err)
		}
	}
	return nil
}

func (s *store) get(ctx context.Context, id string) (*Run, error) {
	f, err := s.db.Do(ctx, s.db.B().Hgetall().Key(runKey(id)).Build()).AsStrMap()
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	if len(f) == 0 {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	return runFromFields(f), nil
}

// claimThread marks the run as the thread's active run; false if one exists.
func (s *store) claimThread(ctx context.Context, threadID, runID string) (bool, error) {
	res := s.db.Do(ctx, s.db.B().Set().Key(threadRunKey(threadID)).Value(runID).Nx().Ex(leaseTTL).Build())
	if err := res.Error(); err != nil {
		if valkey.IsValkeyNil(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *store) refreshLease(ctx context.Context, threadID string) error {
	return s.db.Do(ctx, s.db.B().Expire().Key(threadRunKey(threadID)).Seconds(int64(leaseTTL.Seconds())).Build()).Error()
}

// persistLease keeps the pointer without expiry while a run waits for input.
func (s *store) persistLease(ctx context.Context, threadID string) error {
	return s.db.Do(ctx, s.db.B().Persist().Key(threadRunKey(threadID)).Build()).Error()
}

// releaseScript deletes the thread's active pointer only while it still
// names the run, so a redelivered notice cannot release a newer run.
var releaseScript = valkey.NewLuaScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)

func (s *store) releaseThread(ctx context.Context, threadID, runID string) error {
	return releaseScript.Exec(ctx, s.db, []string{threadRunKey(threadID)}, []string{runID}).Error()
}

// active returns the thread's active run, or nil.
func (s *store) active(ctx context.Context, threadID string) (*Run, error) {
	id, err := s.db.Do(ctx, s.db.B().Get().Key(threadRunKey(threadID)).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, err
	}
	run, err := s.get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return run, err
}

func (s *store) nextSeq(ctx context.Context, threadID string) (int64, error) {
	return s.db.Do(ctx, s.db.B().Incr().Key(threadSeqKey(threadID)).Build()).AsInt64()
}

func (s *store) publish(ctx context.Context, channel, msg string) error {
	return s.db.Do(ctx, s.db.B().Publish().Channel(channel).Message(msg).Build()).Error()
}

// resumeScript moves a paused run to queued only if it is still paused, so
// concurrent answers resume it once.
var resumeScript = valkey.NewLuaScript(`
if redis.call('HGET', KEYS[1], 'status') ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'status', ARGV[2], 'segment_from', ARGV[3])
return 1`)

// resume queues a paused run's next segment from the cursor; false when the
// run was not paused (another answer already resumed it, or it was cancelled).
func (s *store) resume(ctx context.Context, runID, segmentFrom string) (bool, error) {
	n, err := resumeScript.Exec(ctx, s.db, []string{runKey(runID)},
		[]string{string(StatusAwaitingInput), string(StatusQueued), segmentFrom}).AsInt64()
	return n == 1, err
}

// requestCancel marks a running run as cancelled by the user. The worker
// checks it when the run pauses, which the cancel message can race.
func (s *store) requestCancel(ctx context.Context, runID string) error {
	return s.db.Do(ctx, s.db.B().Hset().Key(runKey(runID)).FieldValue().FieldValue("cancel", "1").Build()).Error()
}

func (s *store) cancelRequested(ctx context.Context, runID string) bool {
	v, err := s.db.Do(ctx, s.db.B().Hget().Key(runKey(runID)).Field("cancel").Build()).ToString()
	return err == nil && v == "1"
}

// statuses returns the status of each listed thread's active run, for
// threads that have one: one MGET of the leases, one pipeline of statuses.
func (s *store) statuses(ctx context.Context, threadIDs []string) (map[string]Status, error) {
	out := map[string]Status{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(threadIDs))
	for i, id := range threadIDs {
		keys[i] = threadRunKey(id)
	}
	runIDs, err := s.db.Do(ctx, s.db.B().Mget().Key(keys...).Build()).ToArray()
	if err != nil {
		return nil, err
	}
	var threads []string
	var cmds valkey.Commands
	for i, v := range runIDs {
		if runID, err := v.ToString(); err == nil { // nil reply: no active run
			threads = append(threads, threadIDs[i])
			cmds = append(cmds, s.db.B().Hget().Key(runKey(runID)).Field("status").Build())
		}
	}
	for i, res := range s.db.DoMulti(ctx, cmds...) {
		if status, err := res.ToString(); err == nil {
			out[threads[i]] = Status(status)
		}
	}
	return out, nil
}
