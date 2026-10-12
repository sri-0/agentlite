package runs

import (
	"context"
	"errors"

	"agentlite/internal/stream"
)

// ErrCancelled is the cause of a run's context when the user cancelled it, as
// opposed to the server shutting down.
var ErrCancelled = errors.New("cancelled by the user")

// Cancel stops the thread's active run: a queued or paused run is closed here,
// a running one is told to stop.
func (m *Manager) Cancel(ctx context.Context, threadID string) error {
	run, err := m.store.active(ctx, threadID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNotFound
	}
	switch run.Status {
	case StatusQueued, StatusAwaitingInput:
		return m.closeCancelled(ctx, run)
	}
	// The flag catches a run that pauses before the message reaches its worker.
	if err := m.store.requestCancel(ctx, run.ID); err != nil {
		return err
	}
	return m.store.publish(ctx, ctlChannel(run.ID), "cancel")
}

// closeCancelled ends a run that has no worker executing it: its interrupts
// are closed and a cancelled segment is written, so the stored turn ends as
// cancelled too.
func (m *Manager) closeCancelled(ctx context.Context, run *Run) error {
	if err := m.hitl.Cancel(ctx, run.ThreadID); err != nil {
		return err
	}
	sink := m.sinkFor(ctx, run)
	enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, sink.Emit)
	enc.Start(run.TurnID, run.AgentID)
	enc.Cancel()
	m.settle(sink, StatusCancelled, run.Result, "")
	return nil
}
