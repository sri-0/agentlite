package runs

import (
	"context"
	"iter"
	"time"

	"agentlite/internal/stream"
)

// Subscribe tails the thread's active run from the cursor (after "" means the
// turn's start). It reports idle when there is no active run.
func (m *Manager) Subscribe(ctx context.Context, threadID, after string) (iter.Seq2[Entry, error], error) {
	run, err := m.store.active(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	if after == "" {
		after = run.TurnFrom
	}
	return func(yield func(Entry, error) bool) {
		for entry, err := range m.log.Read(ctx, threadID, after) {
			if !yield(entry, err) || err != nil {
				return
			}
			if stream.Terminal(entry.Event) {
				return
			}
		}
	}, nil
}

// waitPoll is how often Wait checks a run's record.
const waitPoll = 500 * time.Millisecond

// Wait blocks until the run ends, checking its record twice a second. A
// paused run is still running as far as its caller is concerned.
func (m *Manager) Wait(ctx context.Context, runID string) (*Run, error) {
	for {
		run, err := m.store.get(ctx, runID)
		if err != nil {
			return nil, err
		}
		if run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-time.After(waitPoll):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Active returns the thread's active run, or nil.
func (m *Manager) Active(ctx context.Context, threadID string) (*Run, error) {
	return m.store.active(ctx, threadID)
}

// RunStatuses returns the status of each listed thread's active run, for
// threads that have one.
func (m *Manager) RunStatuses(ctx context.Context, threadIDs []string) (map[string]Status, error) {
	return m.store.statuses(ctx, threadIDs)
}
