package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// finalize folds a run's events from the log into its message document, in
// the thread's control set, with ctx's caller: the run's thread token, or
// the user who cancelled a run no worker held. It runs at every pause and at
// the end, upserts by message id, and at the end releases the thread if the
// run still holds it.
func (m *Manager) finalize(ctx context.Context, runID string, status Status) error {
	start := time.Now()
	run, err := m.store.get(ctx, runID)
	if err != nil {
		return err
	}
	entries, err := m.log.Range(ctx, run.ThreadID, run.TurnFrom)
	if err != nil {
		return err
	}
	evs := make([]stream.Event, 0, len(entries))
	for _, e := range entries {
		evs = append(evs, e.Event)
	}
	raws := make([]json.RawMessage, 0, len(evs))
	for _, e := range stream.Coalesce(evs) {
		raw, err := e.ToJSON()
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		raws = append(raws, raw)
	}
	msg := &opensearch.Message{
		ID: run.TurnID, ThreadID: run.ThreadID, RootID: run.RootID, Role: "assistant",
		Seq: run.TurnSeq, Text: run.Result, Events: raws,
		Marks: opensearch.Marks{ControlsetID: run.Controlset, CreatedAt: run.StartedAt}, // the turn's start, at every pause and the end
	}
	saveErr := m.history.SaveMessage(ctx, msg)
	if status.Terminal() {
		// Released even when the save failed: nothing retries it, and the
		// thread must not stay busy. The log keeps the turn's events then.
		m.release(ctx, run)
		if saveErr != nil {
			return saveErr
		}
		if err := m.log.Trim(ctx, run.ThreadID); err != nil {
			m.logger.Warn().Err(err).Str("thread", run.ThreadID).Msg("trim log")
		}
	}
	if saveErr != nil {
		return saveErr
	}
	m.logger.Debug().Str("run", run.ID).Int("events", len(raws)).Dur("took", time.Since(start)).Msg("finalised")
	return nil
}
