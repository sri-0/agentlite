package runs

import (
	"cmp"
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
	remaining, err := m.hitl.Decide(ctx, threadID, interruptID, d)
	switch {
	case errors.Is(err, hitl.ErrNotFound):
		return nil, fmt.Errorf("%w: interrupt %s", ErrNotFound, interruptID)
	case errors.Is(err, hitl.ErrNotPending):
		return nil, fmt.Errorf("%w: interrupt %s was already answered or cancelled", ErrNotPaused, interruptID)
	case err != nil:
		return nil, err
	}
	if run.SegmentFrom, err = m.log.Last(ctx, threadID); err != nil {
		return nil, err
	}
	if remaining > 0 { // the run continues once the rest are answered
		return run, nil
	}
	if err := m.store.update(ctx, run.ID, map[string]string{"segment_from": run.SegmentFrom}); err != nil {
		return nil, err
	}
	resumed, err := m.store.transition(ctx, run.ID, StatusAwaitingInput, StatusQueued)
	if err != nil {
		return nil, err
	}
	if !resumed { // another last answer resumed it, or a cancel closed it
		if now, err := m.store.get(ctx, run.ID); err != nil || now.Status == StatusCancelled {
			return nil, cmp.Or(err, ErrNotPaused)
		}
		return run, nil
	}
	answered, err := m.hitl.TakeDecided(ctx, threadID)
	if err != nil {
		return nil, err
	}
	entries := make([]types.ResumeEntry, len(answered))
	for i := range answered {
		if entries[i], err = answered[i].ResumeEntry(); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	if err := m.store.holdLease(ctx, threadID, queuedLease); err != nil {
		return nil, err
	}
	return run, m.enqueue(ctx, run, map[string]string{"resume": string(raw)})
}

// Interrupts lists the thread's pending interrupts.
func (m *Manager) Interrupts(ctx context.Context, threadID string) ([]hitl.Interrupt, error) {
	return m.hitl.Pending(ctx, threadID)
}
