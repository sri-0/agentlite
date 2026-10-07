package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/internal/config"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// Errors returned to callers.
var (
	ErrNotFound  = errors.New("not found")
	ErrBusy      = errors.New("thread has an active run")
	ErrNotPaused = errors.New("run is not waiting for input")
)

// Engine executes one run segment, reading and writing only Valkey, and seeds
// a thread's session when it is missing.
type Engine interface {
	Run(ctx context.Context, in Input, sink stream.Sink) (Status, string, error)
	Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error
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
	defs     map[string]config.AgentDef
	appName  string
	logger   zerolog.Logger
}

// NewService wires the run lifecycle.
func NewService(db valkey.Client, log Log, sink Sink, history History, sessions session.Service, engine Engine, defs []config.AgentDef, appName string, logger zerolog.Logger) *Service {
	byID := make(map[string]config.AgentDef, len(defs))
	for _, d := range defs {
		byID[d.ID] = d
	}
	return &Service{db: db, runs: &runs{db: db}, log: log, sink: sink, history: history, sessions: sessions, engine: engine, defs: byID, appName: appName, logger: logger}
}

// StartInput describes a new turn.
type StartInput struct {
	ThreadID  string
	ParentID  string // set for a sub-agent's thread
	AgentID   string
	Model     string
	MessageID string
	Text      string
}

const (
	// DefaultAgent answers when no agent is named.
	DefaultAgent = "assistant"
	// sessionUser is adk's required user id; agentlite has no identity yet.
	sessionUser = "default"
	titleLen    = 80

	// A finished run is released by the finalizer; Start waits this long for it.
	finalizeWait = 5 * time.Second
	finalizePoll = 100 * time.Millisecond
)

// Start persists the user message, prepares the thread's session and queues
// the run. Starting the same message id again returns the existing run.
func (s *Service) Start(ctx context.Context, in StartInput) (*Run, error) {
	if in.AgentID == "" {
		in.AgentID = DefaultAgent
	}
	def, ok := s.defs[in.AgentID]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", in.AgentID)
	}
	if in.Model == "" {
		in.Model = def.Model
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
	thread, err := s.upsertThread(ctx, in)
	if err != nil {
		return nil, err
	}
	run.RootID = thread.RootID
	if err := s.ensureSession(ctx, in.ThreadID, in.AgentID); err != nil {
		return nil, err
	}
	seq, err := s.runs.nextSeq(ctx, in.ThreadID)
	if err != nil {
		return nil, err
	}
	if err := s.sink.SaveMessage(ctx, userMessage(thread, in, seq)); err != nil {
		return nil, err
	}
	if run.TurnSeq, err = s.runs.nextSeq(ctx, in.ThreadID); err != nil {
		return nil, err
	}
	if run.TurnFrom, err = s.log.Last(ctx, in.ThreadID); err != nil {
		return nil, err
	}
	run.SegmentFrom = run.TurnFrom
	if err := s.runs.save(ctx, run); err != nil {
		return nil, err
	}
	s.runs.setActive(ctx, run.ID, true)
	return run, s.enqueue(ctx, run, map[string]string{"message": in.Text})
}

// claimThread takes ownership of the thread for the new run. It returns the
// existing run when the same message is sent again, waits briefly for a
// finished run that the finalizer is still persisting, and otherwise reports
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
		case active == nil:
			continue // released between the two reads; claim again
		case active.MessageID == in.MessageID:
			return active, nil
		case active.Status.Terminal() && time.Now().Before(deadline):
			time.Sleep(finalizePoll)
		default:
			return nil, ErrBusy
		}
	}
}

// Resume answers a paused run's interrupts and queues the next segment.
func (s *Service) Resume(ctx context.Context, threadID string, entries []types.ResumeEntry) (*Run, error) {
	run, err := s.runs.active(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status != StatusAwaitingInput {
		return nil, ErrNotPaused
	}
	raw, _ := json.Marshal(entries)
	if run.SegmentFrom, err = s.log.Last(ctx, threadID); err != nil {
		return nil, err
	}
	if err := s.runs.update(ctx, run.ID, map[string]string{"status": string(StatusQueued), "segment_from": run.SegmentFrom}); err != nil {
		return nil, err
	}
	s.runs.refreshLease(ctx, threadID)
	return run, s.enqueue(ctx, run, map[string]string{"resume": string(raw)})
}

// Cancel asks the running segment to stop.
func (s *Service) Cancel(ctx context.Context, threadID string) error {
	run, err := s.runs.active(ctx, threadID)
	if err != nil {
		return err
	}
	if run == nil {
		return ErrNotFound
	}
	s.runs.publish(ctx, ctlChannel(run.ID), "cancel")
	return nil
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

// Wait blocks until the run pauses or ends.
func (s *Service) Wait(ctx context.Context, runID string) (*Run, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	woken := make(chan struct{}, 1)
	go s.db.Receive(ctx, s.db.B().Subscribe().Channel(doneChannel(runID)).Build(), func(valkey.PubSubMessage) {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	for {
		run, err := s.runs.get(ctx, runID)
		if err != nil {
			return nil, err
		}
		if run.Status == StatusAwaitingInput || run.Status.Terminal() {
			return run, nil
		}
		select {
		case <-woken:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ActiveRuns lists runs that still own their thread, keyed by thread id.
func (s *Service) ActiveRuns(ctx context.Context) (map[string]*Run, error) {
	ids, err := s.runs.activeIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]*Run{}
	for _, id := range ids {
		run, err := s.runs.get(ctx, id)
		if err != nil {
			continue
		}
		out[run.ThreadID] = run
	}
	return out, nil
}

// Start runs the worker and finalizer loops in this process.
func (s *Service) StartWorkers(ctx context.Context) {
	for id := range s.defs {
		q := &queue{db: s.db, key: requestsKey + id, group: workerGroup, consumer: s.consumerName(), log: s.logger}
		go q.consume(ctx, s.execute)
	}
	done := &queue{db: s.db, key: doneKey, group: finalGroup, consumer: s.consumerName(), log: s.logger}
	go done.consume(ctx, s.finalize)
}

func (s *Service) consumerName() string { return s.appName + "-" + uuid.NewString()[:8] }

func (s *Service) enqueue(ctx context.Context, run *Run, fields map[string]string) error {
	fields["run_id"] = run.ID
	q := &queue{db: s.db, key: requestsKey + run.AgentID}
	return q.enqueue(ctx, fields)
}

func (s *Service) upsertThread(ctx context.Context, in StartInput) (*opensearch.Thread, error) {
	now := time.Now()
	thread, err := s.history.Thread(ctx, in.ThreadID)
	switch {
	case errors.Is(err, opensearch.ErrNotFound):
		thread = &opensearch.Thread{ID: in.ThreadID, ParentID: in.ParentID, RootID: in.ThreadID, Title: title(in.Text), CreatedAt: now}
		if in.ParentID != "" {
			parent, err := s.history.Thread(ctx, in.ParentID)
			if err != nil {
				return nil, fmt.Errorf("parent thread: %w", err)
			}
			thread.RootID = parent.RootID
		}
	case err != nil:
		return nil, err
	}
	thread.AgentID, thread.Model, thread.UpdatedAt = in.AgentID, in.Model, now
	return thread, s.sink.SaveThread(ctx, thread)
}

func title(text string) string {
	t := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len(t) > titleLen {
		t = t[:titleLen]
	}
	return t
}

// ensureSession rebuilds the thread's adk session from history when missing.
func (s *Service) ensureSession(ctx context.Context, threadID, agentID string) error {
	_, err := s.sessions.Get(ctx, &session.GetRequest{AppName: s.appName, UserID: sessionUser, SessionID: threadID, NumRecentEvents: 1})
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
		raw, _ := stream.Marshal(e)
		raws = append(raws, raw)
	}
	return &opensearch.Message{ID: in.MessageID, ThreadID: thread.ID, RootID: thread.RootID, Role: role, Seq: seq, CreatedAt: time.Now(), Text: in.Text, Events: raws}
}
