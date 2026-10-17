package runs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey keys and lifetimes for run records. A thread's lease names its
// active run: held long while the run waits in a queue, refreshed while a
// worker executes it, kept as long as the record while it waits for input.
const (
	runTTL      = 7 * 24 * time.Hour
	leaseTTL    = 60 * time.Second
	queuedLease = 10 * time.Minute
)

func runKey(id string) string           { return "run:" + id }
func statusKey(id string) string        { return "run:" + id + ":status" }
func threadRunKey(thread string) string { return "thread:" + thread + ":run" }
func threadSeqKey(thread string) string { return "thread:" + thread + ":seq" }
func ctlChannel(runID string) string    { return "run:" + runID + ":ctl" }

// store keeps run records and the per-thread active pointer in Valkey. A
// run's status is a key of its own, moved only by transition, so two parties
// acting on one run (a worker and a cancel, an answer and a cancel) cannot
// both win.
type store struct{ db valkey.Client }

// create stores a new queued run.
func (s *store) create(ctx context.Context, run *Run) error {
	err := s.db.Do(ctx, s.db.B().Set().Key(statusKey(run.ID)).Value(string(StatusQueued)).Nx().Ex(runTTL).Build()).Error()
	if err != nil {
		return fmt.Errorf("run %s: %w", run.ID, err)
	}
	return s.update(ctx, run.ID, run.fields())
}

// update sets fields of the run's record; the status is not one of them.
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

// transition moves the run from one status to another; false when it was no
// longer in from: someone else moved it first.
func (s *store) transition(ctx context.Context, id string, from, to Status) (bool, error) {
	err := s.db.Do(ctx, s.db.B().Set().Key(statusKey(id)).Value(string(to)).Ifeq(string(from)).Ex(runTTL).Build()).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("run %s: %s to %s: %w", id, from, to, err)
	}
	return true, nil
}

func (s *store) get(ctx context.Context, id string) (*Run, error) {
	res := s.db.DoMulti(ctx, s.db.B().Hgetall().Key(runKey(id)).Build(), s.db.B().Get().Key(statusKey(id)).Build())
	f, err := res[0].AsStrMap()
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	status, err := res[1].ToString()
	if len(f) == 0 || valkey.IsValkeyNil(err) {
		return nil, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	run := runFromFields(f)
	run.Status = Status(status)
	return run, nil
}

// claimThread marks the run as the thread's active run; false if one exists.
func (s *store) claimThread(ctx context.Context, threadID, runID string) (bool, error) {
	err := s.db.Do(ctx, s.db.B().Set().Key(threadRunKey(threadID)).Value(runID).Nx().Ex(queuedLease).Build()).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	return err == nil, err
}

// holdLease keeps the thread's lease for ttl more.
func (s *store) holdLease(ctx context.Context, threadID string, ttl time.Duration) error {
	return s.db.Do(ctx, s.db.B().Expire().Key(threadRunKey(threadID)).Seconds(int64(ttl.Seconds())).Build()).Error()
}

// releaseThread deletes the thread's lease only while it still names the
// run, so a redelivered notice cannot release a newer run.
func (s *store) releaseThread(ctx context.Context, threadID, runID string) error {
	return s.db.Do(ctx, s.db.B().Delifeq().Key(threadRunKey(threadID)).Value(runID).Build()).Error()
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

// requestCancel marks a running run as cancelled by the user. The worker
// checks it when it subscribes to cancels and when the run pauses, which the
// cancel message can race.
func (s *store) requestCancel(ctx context.Context, runID string) error {
	return s.db.Do(ctx, s.db.B().Hset().Key(runKey(runID)).FieldValue().FieldValue("cancel", "1").Build()).Error()
}

func (s *store) cancelRequested(ctx context.Context, runID string) (bool, error) {
	v, err := s.db.Do(ctx, s.db.B().Hget().Key(runKey(runID)).Field("cancel").Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	return v == "1", err
}

// statuses returns the status of each listed thread's active run, for
// threads that have one: one MGET of the leases, one of their statuses.
func (s *store) statuses(ctx context.Context, threadIDs []string) (map[string]Status, error) {
	out := map[string]Status{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(threadIDs))
	for i, id := range threadIDs {
		keys[i] = threadRunKey(id)
	}
	runIDs, err := s.db.Do(ctx, s.db.B().Mget().Key(keys...).Build()).AsStrSlice()
	if err != nil {
		return nil, err
	}
	var threads []string
	keys = keys[:0]
	for i, runID := range runIDs {
		if runID != "" { // nil reply: no active run
			threads, keys = append(threads, threadIDs[i]), append(keys, statusKey(runID))
		}
	}
	if len(keys) == 0 {
		return out, nil
	}
	statuses, err := s.db.Do(ctx, s.db.B().Mget().Key(keys...).Build()).AsStrSlice()
	if err != nil {
		return nil, err
	}
	for i, st := range statuses {
		if st != "" {
			out[threads[i]] = Status(st)
		}
	}
	return out, nil
}
