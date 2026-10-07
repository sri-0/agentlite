package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/stream"
)

const leaseRefresh = 20 * time.Second

// execute runs one queued segment. It is the worker loop's handler and talks
// only to Valkey, so it can later run in another process.
func (s *Service) execute(ctx context.Context, fields map[string]string) error {
	run, err := s.runs.get(ctx, fields["run_id"])
	if err != nil {
		s.logger.Warn().Err(err).Msg("request for unknown run; dropped")
		return nil
	}
	if run.Status != StatusQueued {
		// Reclaimed after a crash mid-run: never re-execute, tools have side effects.
		if run.Status == StatusRunning {
			s.settle(ctx, run, StatusError, "", "run was interrupted by a restart")
		}
		return nil
	}
	if err := s.runs.update(ctx, run.ID, map[string]string{"status": string(StatusRunning)}); err != nil {
		return err
	}
	run.Status = StatusRunning

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.watchControl(runCtx, run, cancel)
	go s.keepLease(runCtx, run.ThreadID)

	in := Input{Run: run, Message: fields["message"]}
	if raw := fields["resume"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &in.Resume); err != nil {
			return fmt.Errorf("resume payload: %w", err)
		}
	}
	sink := &appendSink{ctx: ctx, log: s.log, threadID: run.ThreadID, logger: s.logger}
	status, result, err := s.runEngine(runCtx, in, sink)
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	s.settle(ctx, run, status, result, errText)
	return nil
}

// runEngine turns a panic in a run into a failed run instead of a dead process.
func (s *Service) runEngine(ctx context.Context, in Input, sink stream.Sink) (status Status, result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error().Str("run", in.Run.ID).Interface("panic", r).Str("stack", string(debug.Stack())).Msg("run panicked")
			err = fmt.Errorf("internal error: %v", r)
			sink.Emit(events.NewRunErrorEvent(err.Error(), events.WithRunID(in.Run.ID)))
			status = StatusError
		}
	}()
	return s.engine.Run(ctx, in, sink)
}

// settle records the segment's outcome and tells the finalizer. The thread
// stays owned until the finalizer has persisted the turn, so a client never
// sees the thread idle without its answer; the lease expires on its own if
// the finalizer never runs.
func (s *Service) settle(ctx context.Context, run *Run, status Status, result, errText string) {
	if len(result) > resultCap {
		result = result[:resultCap]
	}
	if err := s.runs.update(ctx, run.ID, map[string]string{"status": string(status), "result": result, "error": errText}); err != nil {
		s.logger.Error().Err(err).Str("run", run.ID).Msg("record outcome")
	}
	if status == StatusAwaitingInput {
		s.runs.persistLease(ctx, run.ThreadID)
	}
	done := &queue{db: s.db, key: doneKey}
	if err := done.enqueue(ctx, map[string]string{"run_id": run.ID, "status": string(status)}); err != nil {
		s.logger.Error().Err(err).Str("run", run.ID).Msg("enqueue done")
	}
	s.runs.publish(ctx, doneChannel(run.ID), string(status))
}

func (s *Service) watchControl(ctx context.Context, run *Run, cancel context.CancelFunc) {
	s.db.Receive(ctx, s.db.B().Subscribe().Channel(ctlChannel(run.ID)).Build(), func(m valkey.PubSubMessage) {
		if m.Message == "cancel" {
			cancel()
		}
	})
}

func (s *Service) keepLease(ctx context.Context, threadID string) {
	t := time.NewTicker(leaseRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.runs.refreshLease(ctx, threadID); err != nil {
				s.logger.Error().Err(err).Str("thread", threadID).Msg("refresh lease")
			}
		}
	}
}

// appendSink writes events to the thread log. It uses the worker's context,
// not the cancellable run context, so the closing events of a cancelled run
// still reach the log.
type appendSink struct {
	ctx      context.Context
	log      Log
	threadID string
	logger   zerolog.Logger
}

func (a *appendSink) Emit(e stream.Event) {
	if _, err := a.log.Append(a.ctx, a.threadID, e); err != nil {
		a.logger.Error().Err(err).Str("thread", a.threadID).Msg("append event")
	}
}
