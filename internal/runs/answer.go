package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"

	"agentlite/internal/hitl"
)

// Answer records the user's answer to one of a paused run's interrupts. Once
// every interrupt of the pause is answered, the run's next segment is queued
// with all the answers and the turn continues. Concurrent answers are safe:
// each interrupt takes one answer and the run resumes once.
func (m *Manager) Answer(ctx context.Context, threadID, interruptID string, d hitl.Decision) (*Run, error) {
	if d.Approved == nil && d.Output == nil {
		return nil, fmt.Errorf("%w: an answer needs approved or output", ErrInvalid)
	}
	run, err := m.store.active(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status != StatusAwaitingInput {
		return nil, ErrNotPaused
	}
	rec, err := m.hitl.Get(ctx, interruptID)
	if errors.Is(err, hitl.ErrNotFound) || err == nil && rec.ThreadID != threadID {
		return nil, fmt.Errorf("%w: interrupt %s", ErrNotFound, interruptID)
	}
	if err != nil {
		return nil, err
	}
	remaining, err := m.hitl.Decide(ctx, rec, d)
	if errors.Is(err, hitl.ErrNotPending) {
		return nil, fmt.Errorf("%w: interrupt %s was already answered", ErrNotPaused, interruptID)
	}
	if err != nil {
		return nil, err
	}
	if run.SegmentFrom, err = m.log.Last(ctx, threadID); err != nil {
		return nil, err
	}
	if remaining > 0 { // the run continues once the rest are answered
		return run, nil
	}
	resumed, err := m.store.resume(ctx, run.ID, run.SegmentFrom)
	if err != nil {
		return nil, err
	}
	if !resumed {
		return nil, ErrNotPaused
	}
	answered, err := m.hitl.TakeDecided(ctx, threadID)
	if err != nil {
		return nil, err
	}
	entries := make([]types.ResumeEntry, len(answered))
	for i := range answered {
		entries[i] = answered[i].ResumeEntry()
	}
	raw, _ := json.Marshal(entries)
	if err := m.store.refreshLease(ctx, threadID); err != nil {
		return nil, err
	}
	return run, m.enqueue(ctx, run, map[string]string{"resume": string(raw)})
}

// Interrupts lists the thread's pending interrupts.
func (m *Manager) Interrupts(ctx context.Context, threadID string) ([]hitl.Interrupt, error) {
	return m.hitl.Pending(ctx, threadID)
}
