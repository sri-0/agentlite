package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/adk/v2/session"

	"agentlite/internal/config"
	"agentlite/internal/hitl"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// Errors returned to callers.
var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid request")
	ErrBusy      = errors.New("thread has an active run")
	ErrNotPaused = errors.New("run is not waiting for input")
)

// Engine executes one run segment, reading and writing only Valkey, and seeds
// a thread's session when it is missing.
type Engine interface {
	Run(ctx context.Context, in Input, sink stream.Sink) (Result, error)
	Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error
}

// Result is how a segment ended: its status, the answer text so far, and the
// interrupts it paused on.
type Result struct {
	Status     Status
	Text       string
	Interrupts []types.Interrupt
}

// Input is one segment: a new user message or the answers to interrupts.
type Input struct {
	Run     *Run
	Message string
	Resume  []types.ResumeEntry
}

// Sink persists threads and messages (OpenSearch now, a producer later).
type Sink interface {
	SaveThread(ctx context.Context, t *opensearch.Thread) error
	SaveMessage(ctx context.Context, m *opensearch.Message) error
}

// History reads what the sink persisted.
type History interface {
	Thread(ctx context.Context, id string) (*opensearch.Thread, error)
	Messages(ctx context.Context, threadID string, before int64, limit int) ([]opensearch.Message, error)
}

// Service owns the run lifecycle.
type Service struct {
	db       valkey.Client
	runs     *runs
	log      Log
	sink     Sink
	history  History
	sessions session.Service
	engine   Engine
	hitl     *hitl.Store
	defs     map[string]config.AgentDef
	appName  string
	model    string // the model a run uses when the request names none
	logger   zerolog.Logger
	workers  sync.WaitGroup
}

// NewService wires the run lifecycle.
func NewService(cfg *config.Config, db valkey.Client, sink Sink, history History, sessions session.Service, engine Engine, logger zerolog.Logger) *Service {
	return &Service{
		db: db, runs: &runs{db: db}, log: newValkeyLog(db), sink: sink, history: history, sessions: sessions, engine: engine,
		hitl: hitl.NewStore(db), defs: cfg.Defs(), appName: cfg.AppName, model: cfg.Models.DefaultModel, logger: logger,
	}
}

// StartInput describes a new turn.
type StartInput struct {
	ThreadID   string
	ParentID   string // set for a sub-agent's thread, which Start creates
	AgentID    string
	Model      string
	Connectors *[]string // replaces the thread's choice when set
	MessageID  string
	Text       string
}

const (
	// SessionUser is adk's required user id; agentlite has no identity yet.
	SessionUser = "default"

	// DefaultAgent answers when no agent is named.
	DefaultAgent = "assistant"
	titleLen     = 80

	// A finished run is released by the finalizer; Start waits this long for it.
	finalizeWait = 5 * time.Second
	finalizePoll = 100 * time.Millisecond
	waitPoll     = 500 * time.Millisecond
)

// Start persists the user message, prepares the thread's session and queues
// the run. Starting the same message id again returns the existing run.
func (s *Service) Start(ctx context.Context, in StartInput) (*Run, error) {
	if in.AgentID == "" {
		in.AgentID = DefaultAgent
	}
	if _, ok := s.defs[in.AgentID]; !ok {
		return nil, fmt.Errorf("%w: unknown agent %q", ErrInvalid, in.AgentID)
	}
	if in.Model == "" {
		in.Model = s.model
	}
	if in.MessageID == "" {
		in.MessageID = uuid.NewString()
	}
	run := &Run{
		ID: uuid.Must(uuid.NewV7()).String(), ThreadID: in.ThreadID, AgentID: in.AgentID, Model: in.Model,
		TurnID: uuid.Must(uuid.NewV7()).String(), MessageID: in.MessageID, Status: StatusQueued, StartedAt: time.Now(),
	}
	claimed, err := s.claimThread(ctx, in, run.ID)
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		return claimed, nil
	}
	if err := s.prepare(ctx, in, run); err != nil {
		s.release(ctx, run)
		return nil, err
	}
	return run, nil
}

// prepare persists the user turn, readies the session and queues the run.
func (s *Service) prepare(ctx context.Context, in StartInput, run *Run) error {
	thread, err := s.upsertThread(ctx, in)
	if err != nil {
		return err
	}
	run.RootID, run.Connectors = thread.RootID, thread.Connectors
	if err := s.ensureSession(ctx, in.ThreadID, in.AgentID); err != nil {
		return err
	}
	seq, err := s.runs.nextSeq(ctx, in.ThreadID)
	if err != nil {
		return err
	}
	if err := s.sink.SaveMessage(ctx, userMessage(thread, in, seq)); err != nil {
		return err
	}
	if run.TurnSeq, err = s.runs.nextSeq(ctx, in.ThreadID); err != nil {
		return err
	}
	if run.TurnFrom, err = s.log.Last(ctx, in.ThreadID); err != nil {
		return err
	}
	run.SegmentFrom = run.TurnFrom
	if err := s.runs.save(ctx, run); err != nil {
		return err
	}
	return s.enqueue(ctx, run, map[string]string{"message": in.Text})
}

// claimThread takes ownership of the thread for the new run. It returns the
// existing run when the same message is sent again, waits briefly while the
// previous run is still being prepared or finalised, and otherwise reports
// the thread as busy.
func (s *Service) claimThread(ctx context.Context, in StartInput, runID string) (*Run, error) {
	deadline := time.Now().Add(finalizeWait)
	for {
		claimed, err := s.runs.claimThread(ctx, in.ThreadID, runID)
		if err != nil || claimed {
			return nil, err
		}
		active, err := s.runs.active(ctx, in.ThreadID)
		if err != nil {
			return nil, err
		}
		switch {
		case active != nil && active.MessageID == in.MessageID:
			return active, nil
		case active != nil && !active.Status.Terminal(), time.Now().After(deadline):
			return nil, ErrBusy
		}
		time.Sleep(finalizePoll)
	}
}

// release gives the thread back once its run is over. Errors are logged, not
// returned: the lease expires on its own.
func (s *Service) release(ctx context.Context, run *Run) {
	if err := s.runs.releaseThread(ctx, run.ThreadID, run.ID); err != nil {
		s.logger.Error().Err(err).Str("run", run.ID).Msg("release thread")
	}
}

// Resume answers a paused run's interrupts and queues the next segment. Each
// entry must name one of the thread's pending interrupts; the decision is
// recorded before the run continues.
func (s *Service) Resume(ctx context.Context, threadID string, entries []types.ResumeEntry) (*Run, error) {
	run, err := s.runs.active(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status != StatusAwaitingInput {
		return nil, ErrNotPaused
	}
	for _, entry := range entries {
		if _, err := s.hitl.Decide(ctx, entry.InterruptID, decisionFrom(entry)); err != nil {
			if errors.Is(err, hitl.ErrNotFound) || errors.Is(err, hitl.ErrNotPending) {
				return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
			}
			return nil, err
		}
	}
	raw, _ := json.Marshal(entries)
	if run.SegmentFrom, err = s.log.Last(ctx, threadID); err != nil {
		return nil, err
	}
	if err := s.runs.update(ctx, run.ID, map[string]string{"status": string(StatusQueued), "segment_from": run.SegmentFrom}); err != nil {
		return nil, err
	}
	if err := s.runs.refreshLease(ctx, threadID); err != nil {
		return nil, err
	}
	return run, s.enqueue(ctx, run, map[string]string{"resume": string(raw)})
}

// Decide answers one interrupt by id and resumes the run; the HTTP API's
// form of Resume.
func (s *Service) Decide(ctx context.Context, threadID, interruptID string, d hitl.Decision) (*Run, error) {
	rec, err := s.hitl.Get(ctx, interruptID)
	switch {
	case errors.Is(err, hitl.ErrNotFound):
		return nil, fmt.Errorf("%w: interrupt %s", ErrNotFound, interruptID)
	case err != nil:
		return nil, err
	case rec.ThreadID != threadID:
		return nil, fmt.Errorf("%w: interrupt %s", ErrNotFound, interruptID)
	case rec.Reason == stream.ReasonConfirmation && d.Approved == nil:
		return nil, fmt.Errorf("%w: approved is required", ErrInvalid)
	}
	rec.Decision = &d
	return s.Resume(ctx, threadID, []types.ResumeEntry{rec.ResumeEntry()})
}

// Cancel stops the thread's active run: a queued run is closed here, a
// running one is told to stop, a paused one has its interrupts closed.
func (s *Service) Cancel(ctx context.Context, threadID string) error {
	run, err := s.runs.active(ctx, threadID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNotFound
	}
	switch run.Status {
	case StatusQueued:
		enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, s.sinkFor(ctx, run).Emit)
		enc.Start(run.TurnID, run.AgentID)
		enc.Cancel()
		s.settle(ctx, run, StatusCancelled, "", "")
	case StatusAwaitingInput:
		if err := s.hitl.Cancel(ctx, threadID); err != nil {
			return err
		}
		s.settle(ctx, run, StatusCancelled, run.Result, "")
	default:
		s.runs.publish(ctx, ctlChannel(run.ID), "cancel")
	}
	return nil
}

// Interrupts lists the thread's pending interrupts.
func (s *Service) Interrupts(ctx context.Context, threadID string) ([]hitl.Interrupt, error) {
	return s.hitl.Pending(ctx, threadID)
}

// decisionFrom reads the user's decision out of an AG-UI resume entry.
func decisionFrom(entry types.ResumeEntry) hitl.Decision {
	payload, ok := entry.Payload.(map[string]any)
	if !ok {
		return hitl.Decision{Output: entry.Payload}
	}
	if approved, ok := payload["approved"].(bool); ok {
		justification, _ := payload["justification"].(string)
		return hitl.Decision{Approved: &approved, Justification: justification}
	}
	return hitl.Decision{Output: payload}
}

// Subscribe tails the thread's active run from the cursor (after "" means the
// turn's start). It reports idle when there is no active run.
func (s *Service) Subscribe(ctx context.Context, threadID, after string) (iter.Seq2[Entry, error], error) {
	run, err := s.runs.active(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	if after == "" {
		after = run.TurnFrom
	}
	return func(yield func(Entry, error) bool) {
		for entry, err := range s.log.Read(ctx, threadID, after) {
			if !yield(entry, err) || err != nil {
				return
			}
			if stream.Terminal(entry.Event) {
				return
			}
		}
	}, nil
}

// Wait blocks until the run pauses or ends, checking its record twice a second.
func (s *Service) Wait(ctx context.Context, runID string) (*Run, error) {
	for {
		run, err := s.runs.get(ctx, runID)
		if err != nil {
			return nil, err
		}
		if run.Status == StatusAwaitingInput || run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-time.After(waitPoll):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Active returns the thread's active run, or nil.
func (s *Service) Active(ctx context.Context, threadID string) (*Run, error) {
	return s.runs.active(ctx, threadID)
}

// RunStatuses returns the status of each listed thread's active run, for
// threads that have one.
func (s *Service) RunStatuses(ctx context.Context, threadIDs []string) (map[string]Status, error) {
	out := map[string]Status{}
	if len(threadIDs) == 0 {
		return out, nil
	}
	keys := make([]string, len(threadIDs))
	for i, id := range threadIDs {
		keys[i] = threadRunKey(id)
	}
	runIDs, err := s.db.Do(ctx, s.db.B().Mget().Key(keys...).Build()).ToArray()
	if err != nil {
		return nil, err
	}
	for i, v := range runIDs {
		runID, err := v.ToString()
		if err != nil {
			continue // no active run (nil reply)
		}
		status, err := s.db.Do(ctx, s.db.B().Hget().Key(runKey(runID)).Field("status").Build()).ToString()
		if err == nil {
			out[threadIDs[i]] = Status(status)
		}
	}
	return out, nil
}

// StartWorkers runs the worker and finalizer loops in this process until ctx
// ends; Stop then waits for the runs they are executing to settle.
func (s *Service) StartWorkers(ctx context.Context) {
	for id := range s.defs {
		s.consume(ctx, &queue{db: s.db, key: requestsKey + id, group: workerGroup, consumer: s.consumerName(), log: s.logger}, s.execute)
	}
	s.consume(ctx, &queue{db: s.db, key: doneKey, group: finalGroup, consumer: s.consumerName(), log: s.logger}, s.finalize)
}

func (s *Service) consume(ctx context.Context, q *queue, handler func(context.Context, map[string]string) error) {
	s.workers.Go(func() { q.consume(ctx, handler) })
}

// Stop waits for the worker loops to finish their current entries.
func (s *Service) Stop() { s.workers.Wait() }

func (s *Service) consumerName() string { return s.appName + "-" + uuid.NewString()[:8] }

func (s *Service) enqueue(ctx context.Context, run *Run, fields map[string]string) error {
	fields["run_id"] = run.ID
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(fields)) // the worker continues this trace
	q := &queue{db: s.db, key: requestsKey + run.AgentID}
	return q.enqueue(ctx, fields)
}

// CreateThread makes an empty thread with a server id; its title is set by
// its first turn. Runs only start on threads that exist.
func (s *Service) CreateThread(ctx context.Context, connectors *[]string) (*opensearch.Thread, error) {
	now := time.Now()
	id := uuid.Must(uuid.NewV7()).String()
	thread := &opensearch.Thread{ID: id, RootID: id, Connectors: connectors, CreatedAt: now, UpdatedAt: now}
	return thread, s.sink.SaveThread(ctx, thread)
}

// upsertThread records the turn on its thread. A sub-agent's thread is
// created here under its parent's root; any other thread must already exist.
func (s *Service) upsertThread(ctx context.Context, in StartInput) (*opensearch.Thread, error) {
	now := time.Now()
	thread, err := s.history.Thread(ctx, in.ThreadID)
	switch {
	case errors.Is(err, opensearch.ErrNotFound) && in.ParentID != "":
		parent, err := s.history.Thread(ctx, in.ParentID)
		if err != nil {
			return nil, fmt.Errorf("parent thread: %w", err)
		}
		thread = &opensearch.Thread{ID: in.ThreadID, ParentID: in.ParentID, RootID: parent.RootID, CreatedAt: now}
	case errors.Is(err, opensearch.ErrNotFound):
		return nil, fmt.Errorf("%w: thread %s", ErrNotFound, in.ThreadID)
	case err != nil:
		return nil, err
	}
	if in.Connectors != nil {
		thread.Connectors = in.Connectors
	}
	if thread.Title == "" {
		thread.Title = title(in.Text)
	}
	thread.AgentID, thread.Model, thread.UpdatedAt = in.AgentID, in.Model, now
	return thread, s.sink.SaveThread(ctx, thread)
}

func title(text string) string {
	t := []rune(strings.TrimSpace(strings.SplitN(text, "\n", 2)[0]))
	if len(t) > titleLen {
		t = t[:titleLen]
	}
	return string(t)
}

// ensureSession rebuilds the thread's adk session from history when missing.
func (s *Service) ensureSession(ctx context.Context, threadID, agentID string) error {
	_, err := s.sessions.Get(ctx, &session.GetRequest{AppName: s.appName, UserID: SessionUser, SessionID: threadID, NumRecentEvents: 1})
	if err == nil {
		return nil
	}
	if !errors.Is(err, session.ErrNotFound) {
		return err
	}
	history, err := s.history.Messages(ctx, threadID, 0, 500)
	if err != nil {
		return err
	}
	return s.engine.Seed(ctx, threadID, agentID, history)
}

// userMessage stores the user turn as a one-message AG-UI event sequence.
func userMessage(thread *opensearch.Thread, in StartInput, seq int64) *opensearch.Message {
	role := "user"
	evs := []stream.Event{
		events.NewTextMessageStartEvent(in.MessageID, events.WithRole(role)),
		events.NewTextMessageContentEvent(in.MessageID, in.Text),
		events.NewTextMessageEndEvent(in.MessageID),
	}
	raws := make([]json.RawMessage, 0, len(evs))
	for _, e := range evs {
		raw, _ := e.ToJSON()
		raws = append(raws, raw)
	}
	return &opensearch.Message{ID: in.MessageID, ThreadID: thread.ID, RootID: thread.RootID, Role: role, Seq: seq, CreatedAt: time.Now(), Text: in.Text, Events: raws}
}
