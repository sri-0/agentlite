package runs

import (
	"context"
	"errors"

	"agentlite/internal/stream"
	"agentlite/pkg/auth"
)

// ErrCancelled is the cause of a run's context when the user cancelled it, as
// opposed to the server shutting down.
var ErrCancelled = errors.New("cancelled by the user")

// Cancel stops the thread's active run: a queued or paused run is closed here,
// a running one is told to stop. Only the thread's creator may.
func (m *Manager) Cancel(ctx context.Context, threadID string) error {
	if err := m.own(ctx, threadID); err != nil {
		return err
	}
	run, err := m.store.active(ctx, threadID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNotFound
	}
	switch run.Status {
	case StatusQueued, StatusAwaitingInput:
		if closed, err := m.closeCancelled(ctx, run, run.Status); closed || err != nil {
			return err
		}
		// A worker took it, or an answer resumed it: stop it as a running run.
	}
	// The flag catches a run that pauses, or subscribes, before the message
	// reaches its worker.
	if err := m.store.requestCancel(ctx, run.ID); err != nil {
		return err
	}
	return m.store.publish(ctx, ctlChannel(run.ID), "cancel")
}

// closeCancelled ends a run no worker is executing, if it is still in from:
// its interrupts are closed and a cancelled segment is written, so the stored
// turn ends as cancelled too. False when the run moved first.
func (m *Manager) closeCancelled(ctx context.Context, run *Run, from Status) (bool, error) {
	if closed, err := m.store.transition(ctx, run.ID, from, StatusCancelled); !closed || err != nil {
		return false, err
	}
	if err := m.hitl.Cancel(ctx, run.ThreadID); err != nil {
		return true, err
	}
	sink := m.sinkFor(ctx, run)
	enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, sink.Emit)
	enc.Start(run.TurnID, run.AgentID)
	enc.Cancel()
	m.record(sink, StatusCancelled, run.Result, "")
	if u := auth.From(ctx); u != nil {
		m.audit(run, *u).Str("status", string(StatusCancelled)).Msg("run ended")
	}
	return true, nil
}
