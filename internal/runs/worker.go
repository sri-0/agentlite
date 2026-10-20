package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
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
	"agentlite/pkg/db/opensearch"
)

const leaseRefresh = 20 * time.Second

var tracer = otel.Tracer("agentlite")

// execute runs one queued segment. It is the worker loop's handler. Every
// OpenSearch call of the segment, its finalizing included, is made with the
// thread token of the user who queued it (caller), which lives in this
// process's memory only.
func (m *Manager) execute(ctx context.Context, fields map[string]string) error {
	run, err := m.store.get(ctx, fields["run_id"])
	if err != nil {
		m.logger.Warn().Err(err).Msg("request for unknown run; dropped")
		return nil
	}
	user := entryUser(fields)
	caller, callerErr := m.caller(ctx, run, user)
	// Closing writes must land even during shutdown.
	bg := opensearch.WithCaller(context.WithoutCancel(ctx), caller)
	if run.Status != StatusQueued {
		m.redelivered(bg, run)
		return nil
	}
	if started, err := m.store.transition(ctx, run.ID, StatusQueued, StatusRunning); !started || err != nil {
		return err // not started: cancelled while queued
	}
	run.Status = StatusRunning
	if callerErr != nil { // no thread token: the run fails before it starts
		m.audit(run, user).Err(callerErr).Str("status", string(StatusError)).Msg("run refused: no thread token")
		sink := m.sinkFor(bg, run)
		msg := fmt.Sprintf("%v; the run could not reach the conversation's records", callerErr)
		sink.Emit(events.NewRunErrorEvent(msg, events.WithRunID(run.ID)))
		m.settle(sink, StatusRunning, StatusError, "", msg)
		return nil
	}
	m.audit(run, user).Bool("resumed", fields["resume"] != "").Msg("run started")

	runCtx, span := tracer.Start(otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(fields)), "run", trace.WithAttributes(
		attribute.String("thread.id", run.ThreadID), attribute.String("run.id", run.ID), attribute.String("agent.id", run.AgentID)))
	defer span.End()
	runCtx, cancel := context.WithCancelCause(opensearch.WithCaller(runCtx, caller))
	defer cancel(nil)
	go m.watchControl(runCtx, run, cancel)
	go m.keepLease(runCtx, run.ThreadID)

	in := Input{Run: run, Message: fields["message"], User: user}
	if raw := fields["resume"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &in.Resume); err != nil {
			return fmt.Errorf("resume payload: %w", err)
		}
	}
	sink := m.sinkFor(bg, run)
	result, err := m.runEngine(runCtx, in, sink)
	cancel(nil) // stop refreshing the lease before settle holds it for the pause
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	if result.Status == StatusAwaitingInput {
		cancelled, err := m.store.cancelRequested(bg, run.ID)
		if err != nil {
			m.logger.Error().Err(err).Str("run", run.ID).Msg("read cancel flag")
		}
		if cancelled { // cancelled while it was pausing; the pause is never announced
			_, err := m.closeCancelled(bg, run, StatusRunning)
			return err
		}
		if err := m.hitl.Add(bg, run.ThreadID, run.ID, result.Interrupts); err != nil {
			// A pause whose interrupts are not recorded could never be answered.
			result.Status, errText, sink.pause = StatusError, fmt.Sprintf("record interrupts: %v", err), nil
			sink.Emit(events.NewRunErrorEvent(errText, events.WithRunID(run.ID)))
		}
	}
	m.settle(sink, StatusRunning, result.Status, result.Text, errText)
	m.audit(run, user).Str("status", string(result.Status)).Str("error", errText).Msg("run ended")
	return nil
}

// redelivered handles a request whose run is no longer queued: cancelled
// before a worker took it, still running on another worker, or left running
// by a worker that died. A run is never executed twice: tools have side
// effects.
func (m *Manager) redelivered(ctx context.Context, run *Run) {
	if run.Status != StatusRunning {
		return
	}
	active, err := m.store.active(ctx, run.ThreadID)
	switch {
	case err != nil:
		m.logger.Error().Err(err).Str("run", run.ID).Msg("redelivered run")
	case active != nil && active.ID == run.ID:
		// Its worker is alive (it keeps the lease): the run is just long.
	case active != nil:
		// The thread has moved on; its log belongs to the newer run now.
		if _, err := m.store.transition(ctx, run.ID, StatusRunning, StatusError); err != nil {
			m.logger.Error().Err(err).Str("run", run.ID).Msg("close interrupted run")
		}
	default:
		const msg = "run was interrupted by a restart"
		sink := m.sinkFor(ctx, run)
		sink.Emit(events.NewRunErrorEvent(msg, events.WithRunID(run.ID)))
		m.settle(sink, StatusRunning, StatusError, "", msg)
	}
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

// settle moves the run from its current status to the segment's outcome and
// records it (see record). A run that moved meanwhile is left as it is.
func (m *Manager) settle(sink *appendSink, from, to Status, result, errText string) {
	if moved, err := m.store.transition(sink.ctx, sink.run.ID, from, to); !moved || err != nil {
		m.logger.Error().Err(err).Str("run", sink.run.ID).Str("from", string(from)).Str("to", string(to)).Msg("settle run")
		return
	}
	m.record(sink, to, result, errText)
}

// record stores the segment's outcome, announces a held pause now that it is
// recorded, and finalizes the turn here, in the process that ran it: only
// it holds the thread token the turn's message is saved with. The thread
// stays owned until the turn is persisted, so a client never sees the thread
// idle without its answer; the lease expires on its own if finalizing fails.
// A paused run keeps it as long as its record.
func (m *Manager) record(sink *appendSink, status Status, result, errText string) {
	ctx, run := sink.ctx, sink.run
	if len(result) > resultCap {
		result = strings.ToValidUTF8(result[:resultCap], "") // not half a character
	}
	if err := m.store.update(ctx, run.ID, map[string]string{"result": result, "error": errText}); err != nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("record outcome")
	}
	if status == StatusAwaitingInput {
		if err := m.store.holdLease(ctx, run.ThreadID, runTTL); err != nil {
			m.logger.Error().Err(err).Str("run", run.ID).Msg("hold lease for the pause")
		}
	}
	sink.announcePause()
	if err := m.finalize(ctx, run.ID, status); err != nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("finalize")
	}
}

// watchControl cancels the run when the user asks. A cancel published before
// the subscription was live is caught by the flag Cancel sets first.
func (m *Manager) watchControl(ctx context.Context, run *Run, cancel context.CancelCauseFunc) {
	subscribed := valkey.WithOnSubscriptionHook(ctx, func(s valkey.PubSubSubscription) {
		if s.Kind != "subscribe" {
			return
		}
		go func() { // not on the client's reader
			requested, err := m.store.cancelRequested(ctx, run.ID)
			if err != nil && ctx.Err() == nil {
				m.logger.Error().Err(err).Str("run", run.ID).Msg("read cancel flag")
			}
			if requested {
				cancel(ErrCancelled)
			}
		}()
	})
	subscribe := m.db.B().Subscribe().Channel(ctlChannel(run.ID)).Build()
	err := m.db.Receive(subscribed, subscribe, func(msg valkey.PubSubMessage) {
		if msg.Message == "cancel" {
			cancel(ErrCancelled)
		}
	})
	if err != nil && ctx.Err() == nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("cancel subscription lost")
	}
}

// keepLease holds the thread for the run while it executes, from the
// queued run's longer lease down to leaseTTL.
func (m *Manager) keepLease(ctx context.Context, threadID string) {
	t := time.NewTicker(leaseRefresh)
	defer t.Stop()
	for {
		if err := m.store.holdLease(ctx, threadID, leaseTTL); err != nil && ctx.Err() == nil {
			m.logger.Error().Err(err).Str("thread", threadID).Msg("refresh lease")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// appendSink writes a run's events to the thread log. Its context outlives
// the run, so the closing events of a cancelled run or a shutdown still land.
// The event that announces a pause is held until record has stored the
// pause: a client may answer the moment it sees the interrupt, and must find
// the run waiting.
type appendSink struct {
	ctx    context.Context
	log    *valkeyLog
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

// StartWorkers creates the queues' consumer groups, then runs the worker
// loops in this process until ctx ends; Stop then waits for the runs they are
// executing to settle. A group made before any request can arrive starts at
// the stream's beginning, so no request is skipped.
func (m *Manager) StartWorkers(ctx context.Context) error {
	for id := range m.defs {
		q := &queue{db: m.db, key: requestsKey + id, group: workerGroup, consumer: m.consumerName(), log: m.logger}
		if err := q.createGroup(ctx); err != nil {
			return err
		}
		m.workers.Go(func() { q.consume(ctx, m.execute) })
	}
	return nil
}

// Stop waits for the worker loops to finish their current entries.
func (m *Manager) Stop() { m.workers.Wait() }

func (m *Manager) consumerName() string { return m.appName + "-" + uuid.NewString()[:8] }
