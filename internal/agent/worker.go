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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"agentlite/internal/stream"
)

const leaseRefresh = 20 * time.Second

var tracer = otel.Tracer("agentlite")

// execute runs one queued segment. It is the worker loop's handler and talks
// only to Valkey, so it can later run in another process.
func (s *Service) execute(ctx context.Context, fields map[string]string) error {
	run, err := s.runs.get(ctx, fields["run_id"])
	if err != nil {
		s.logger.Warn().Err(err).Msg("request for unknown run; dropped")
		return nil
	}
	bg := context.WithoutCancel(ctx) // closing writes must land even during shutdown
	if run.Status != StatusQueued {
		// Reclaimed while running: if the thread lease still names the run, its
		// worker is alive and the run is just long; otherwise the worker died.
		// Never re-execute either way, tools have side effects.
		active, err := s.runs.active(ctx, run.ThreadID)
		s.logger.Debug().Str("run", run.ID).Str("status", string(run.Status)).Err(err).Msg("run delivered again")
		if err == nil && active != nil && active.ID == run.ID {
			return nil
		}
		if run.Status == StatusRunning {
			const msg = "run was interrupted by a restart"
			s.sinkFor(bg, run).Emit(events.NewRunErrorEvent(msg, events.WithRunID(run.ID)))
			s.settle(bg, run, StatusError, "", msg)
		}
		return nil
	}
	if err := s.runs.update(ctx, run.ID, map[string]string{"status": string(StatusRunning)}); err != nil {
		return err
	}
	run.Status = StatusRunning

	runCtx, span := tracer.Start(otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(fields)), "run", trace.WithAttributes(
		attribute.String("thread.id", run.ThreadID), attribute.String("run.id", run.ID), attribute.String("agent.id", run.AgentID)))
	defer span.End()
	runCtx, cancel := context.WithCancel(runCtx)
	defer cancel()
	go s.watchControl(runCtx, run, cancel)
	go s.keepLease(runCtx, run.ThreadID)

	in := Input{Run: run, Message: fields["message"]}
	if raw := fields["resume"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &in.Resume); err != nil {
			return fmt.Errorf("resume payload: %w", err)
		}
	}
	result, err := s.runEngine(runCtx, in, s.sinkFor(bg, run))
	cancel() // stop refreshing the lease before settle may persist it
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if result.Status == StatusAwaitingInput {
		if err := s.hitl.Add(bg, run.ThreadID, run.ID, result.Interrupts); err != nil {
			s.logger.Error().Err(err).Str("run", run.ID).Msg("record interrupts")
		}
	}
	s.settle(bg, run, result.Status, result.Text, errText)
	return nil
}

// runEngine turns a panic in a run into a failed run instead of a dead process.
func (s *Service) runEngine(ctx context.Context, in Input, sink stream.Sink) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error().Str("run", in.Run.ID).Interface("panic", r).Str("stack", string(debug.Stack())).Msg("run panicked")
			err = fmt.Errorf("internal error: %v", r)
			sink.Emit(events.NewRunErrorEvent(err.Error(), events.WithRunID(in.Run.ID)))
			result = Result{Status: StatusError}
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
		if err := s.runs.persistLease(ctx, run.ThreadID); err != nil {
			s.logger.Error().Err(err).Str("run", run.ID).Msg("persist lease")
		}
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

// appendSink writes a run's events to the thread log. Its context outlives
// the run, so the closing events of a cancelled run or a shutdown still land.
type appendSink struct {
	ctx    context.Context
	log    Log
	run    *Run
	logger zerolog.Logger
}

func (s *Service) sinkFor(ctx context.Context, run *Run) *appendSink {
	return &appendSink{ctx: ctx, log: s.log, run: run, logger: s.logger}
}

func (a *appendSink) Emit(e stream.Event) {
	if _, err := a.log.Append(a.ctx, a.run.ThreadID, a.run.ID, a.run.AgentID, e); err != nil {
		a.logger.Error().Err(err).Str("thread", a.run.ThreadID).Msg("append event")
	}
}
