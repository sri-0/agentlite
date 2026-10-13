package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/google/uuid"
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
func (m *Manager) execute(ctx context.Context, fields map[string]string) error {
	run, err := m.store.get(ctx, fields["run_id"])
	if err != nil {
		m.logger.Warn().Err(err).Msg("request for unknown run; dropped")
		return nil
	}
	bg := context.WithoutCancel(ctx) // closing writes must land even during shutdown
	if run.Status != StatusQueued {
		// Reclaimed while running: if the thread lease still names the run, its
		// worker is alive and the run is just long; otherwise the worker died.
		// Never re-execute either way, tools have side effects.
		active, err := m.store.active(ctx, run.ThreadID)
		m.logger.Debug().Str("run", run.ID).Str("status", string(run.Status)).Err(err).Msg("run delivered again")
		if err == nil && active != nil && active.ID == run.ID {
			return nil
		}
		if run.Status == StatusRunning {
			const msg = "run was interrupted by a restart"
			sink := m.sinkFor(bg, run)
			sink.Emit(events.NewRunErrorEvent(msg, events.WithRunID(run.ID)))
			m.settle(sink, StatusError, "", msg)
		}
		return nil
	}
	if err := m.store.update(ctx, run.ID, map[string]string{"status": string(StatusRunning)}); err != nil {
		return err
	}
	run.Status = StatusRunning

	runCtx, span := tracer.Start(otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(fields)), "run", trace.WithAttributes(
		attribute.String("thread.id", run.ThreadID), attribute.String("run.id", run.ID), attribute.String("agent.id", run.AgentID)))
	defer span.End()
	runCtx, cancel := context.WithCancelCause(runCtx)
	defer cancel(nil)
	go m.watchControl(runCtx, run, cancel)
	go m.keepLease(runCtx, run.ThreadID)

	in := Input{Run: run, Message: fields["message"], Authorization: fields["authorization"]}
	if raw := fields["resume"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &in.Resume); err != nil {
			return fmt.Errorf("resume payload: %w", err)
		}
	}
	sink := m.sinkFor(bg, run)
	result, err := m.runEngine(runCtx, in, sink)
	cancel(nil) // stop refreshing the lease before settle may persist it
	if result.Status == StatusAwaitingInput && m.store.cancelRequested(bg, run.ID) {
		return m.closeCancelled(bg, run) // cancelled while it was pausing; the pause is never announced
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if result.Status == StatusAwaitingInput {
		if err := m.hitl.Add(bg, run.ThreadID, run.ID, result.Interrupts); err != nil {
			m.logger.Error().Err(err).Str("run", run.ID).Msg("record interrupts")
		}
	}
	m.settle(sink, result.Status, result.Text, errText)
	return nil
}

// runEngine turns a panic in a run into a failed run instead of a dead process.
func (m *Manager) runEngine(ctx context.Context, in Input, sink stream.Sink) (result Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error().Str("run", in.Run.ID).Interface("panic", r).Str("stack", string(debug.Stack())).Msg("run panicked")
			err = fmt.Errorf("internal error: %v", r)
			sink.Emit(events.NewRunErrorEvent(err.Error(), events.WithRunID(in.Run.ID)))
			result = Result{Status: StatusError}
		}
	}()
	return m.runtime.Run(ctx, in, sink)
}

// settle records the segment's outcome, announces a held pause now that it is
// recorded, and tells the finalizer. The thread stays owned until the
// finalizer has persisted the turn, so a client never sees the thread idle
// without its answer; the lease expires on its own if the finalizer never runs.
func (m *Manager) settle(sink *appendSink, status Status, result, errText string) {
	ctx, run := sink.ctx, sink.run
	if len(result) > resultCap {
		result = result[:resultCap]
	}
	if err := m.store.update(ctx, run.ID, map[string]string{"status": string(status), "result": result, "error": errText}); err != nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("record outcome")
	}
	if status == StatusAwaitingInput {
		if err := m.store.persistLease(ctx, run.ThreadID); err != nil {
			m.logger.Error().Err(err).Str("run", run.ID).Msg("persist lease")
		}
	}
	sink.announcePause()
	done := &queue{db: m.db, key: doneKey}
	if err := done.enqueue(ctx, map[string]string{"run_id": run.ID, "status": string(status)}); err != nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("enqueue done")
	}
}

func (m *Manager) watchControl(ctx context.Context, run *Run, cancel context.CancelCauseFunc) {
	m.db.Receive(ctx, m.db.B().Subscribe().Channel(ctlChannel(run.ID)).Build(), func(msg valkey.PubSubMessage) {
		if msg.Message == "cancel" {
			cancel(ErrCancelled)
		}
	})
}

func (m *Manager) keepLease(ctx context.Context, threadID string) {
	t := time.NewTicker(leaseRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.store.refreshLease(ctx, threadID); err != nil {
				m.logger.Error().Err(err).Str("thread", threadID).Msg("refresh lease")
			}
		}
	}
}

// appendSink writes a run's events to the thread log. Its context outlives
// the run, so the closing events of a cancelled run or a shutdown still land.
// The event that announces a pause is held until settle has recorded the
// pause: a client may answer the moment it sees the interrupt, and must find
// the run waiting.
type appendSink struct {
	ctx    context.Context
	log    Log
	run    *Run
	logger zerolog.Logger
	pause  stream.Event
}

func (m *Manager) sinkFor(ctx context.Context, run *Run) *appendSink {
	return &appendSink{ctx: ctx, log: m.log, run: run, logger: m.logger}
}

func (a *appendSink) Emit(e stream.Event) {
	if f, ok := e.(*events.RunFinishedEvent); ok && f.Outcome != nil && f.Outcome.Type == events.RunFinishedOutcomeTypeInterrupt {
		a.pause = e
		return
	}
	a.append(e)
}

func (a *appendSink) announcePause() {
	if a.pause != nil {
		a.append(a.pause)
		a.pause = nil
	}
}

func (a *appendSink) append(e stream.Event) {
	if _, err := a.log.Append(a.ctx, a.run.ThreadID, e); err != nil {
		a.logger.Error().Err(err).Str("thread", a.run.ThreadID).Msg("append event")
	}
}

// StartWorkers runs the worker and finalizer loops in this process until ctx
// ends; Stop then waits for the runs they are executing to settle.
func (m *Manager) StartWorkers(ctx context.Context) {
	for id := range m.defs {
		m.consume(ctx, &queue{db: m.db, key: requestsKey + id, group: workerGroup, consumer: m.consumerName(), log: m.logger}, m.execute)
	}
	m.consume(ctx, &queue{db: m.db, key: doneKey, group: finalGroup, consumer: m.consumerName(), log: m.logger}, m.finalize)
}

func (m *Manager) consume(ctx context.Context, q *queue, handler func(context.Context, map[string]string) error) {
	m.workers.Go(func() { q.consume(ctx, handler) })
}

// Stop waits for the worker loops to finish their current entries.
func (m *Manager) Stop() { m.workers.Wait() }

func (m *Manager) consumerName() string { return m.appName + "-" + uuid.NewString()[:8] }
