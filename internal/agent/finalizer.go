package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// finalize folds a run's events from the log into its message document. It
// runs at every pause and at the end, upserts by message id, releases the
// thread only if the run still holds it, and never reads worker memory.
func (s *Service) finalize(ctx context.Context, fields map[string]string) error {
	start := time.Now()
	run, err := s.runs.get(ctx, fields["run_id"])
	if err != nil {
		s.logger.Warn().Err(err).Msg("done notice for unknown run; dropped")
		return nil
	}
	entries, err := s.log.Range(ctx, run.ThreadID, run.TurnFrom)
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
		Seq: run.TurnSeq, CreatedAt: run.StartedAt, Text: run.Result, Events: raws,
	}
	if err := s.sink.SaveMessage(ctx, msg); err != nil {
		return err
	}
	if Status(fields["status"]).Terminal() {
		s.release(ctx, run)
		if err := s.log.Trim(ctx, run.ThreadID); err != nil {
			s.logger.Warn().Err(err).Str("thread", run.ThreadID).Msg("trim log")
		}
	}
	s.logger.Debug().Str("run", run.ID).Int("events", len(raws)).Dur("took", time.Since(start)).Msg("finalised")
	return nil
}
