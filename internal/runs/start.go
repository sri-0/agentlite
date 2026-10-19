package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"

	"agentlite/config"
	"agentlite/internal/auth"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// StartInput describes a new turn.
type StartInput struct {
	newThread  bool // set by Start when it gave the turn a new thread
	ThreadID   string
	ParentID   string // set for a sub-agent's thread, which Start creates
	AgentID    string
	Model      string
	Connectors *[]string // replaces the thread's choice when set
	ImageModel string    // the image model the turn's generate_image calls use; "" is the default
	// WorkspaceID files a new conversation under a workspace; the caller
	// checks it exists.
	WorkspaceID string
	MessageID   string
	Text        string
	// User is who starts the turn, with their access token: a user's own
	// turn mints the conversation's thread token for them from it, the
	// worker too when it keeps none, and the run's tools forward it to the
	// systems they call (connectors with http.forward). A sub-agent's turn
	// carries its parent's.
	User auth.Principal
}

const (
	titleLen = 80
	// A finished run is released by the finalizer; Start waits this long for it.
	finalizeWait = 5 * time.Second
	finalizePoll = 100 * time.Millisecond
)

// Start persists the user message, prepares the thread's session and queues
// the run. Starting the same message id again returns the existing run.
func (m *Manager) Start(ctx context.Context, in StartInput) (*Run, error) {
	if in.AgentID == "" {
		in.AgentID = config.DefaultAgent
	}
	def, ok := m.defs[in.AgentID]
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: unknown agent %q", ErrInvalid, in.AgentID)
	case in.ParentID == "" && !def.Primary():
		return nil, fmt.Errorf("%w: agent %q only runs as a sub-agent", ErrInvalid, in.AgentID)
	}
	if in.Text == "" {
		return nil, fmt.Errorf("%w: a run needs a message", ErrInvalid)
	}
	if in.Connectors != nil {
		if err := m.runtime.CheckConnectors(*in.Connectors); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if in.ThreadID == "" { // a new conversation: the thread id is the server's
		in.ThreadID, in.newThread = uuid.Must(uuid.NewV7()).String(), true
	}
	if in.Model == "" {
		in.Model = m.model
	}
	if err := m.runtime.CheckModel(in.Model); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if in.MessageID == "" {
		in.MessageID = uuid.Must(uuid.NewV7()).String()
	}
	run := &Run{
		ID: uuid.Must(uuid.NewV7()).String(), ThreadID: in.ThreadID, AgentID: in.AgentID, Model: in.Model,
		ImageModel: in.ImageModel,
		TurnID:     uuid.Must(uuid.NewV7()).String(), MessageID: in.MessageID, Status: StatusQueued, StartedAt: time.Now(),
	}
	claimed, err := m.claimThread(ctx, in, run.ID)
	if err != nil {
		return nil, err
	}
	if claimed != nil {
		return claimed, nil
	}
	if err := m.prepare(ctx, in, run); err != nil {
		m.release(ctx, run)
		return nil, err
	}
	return run, nil
}

// prepare persists the user turn, readies the session and queues the run. A
// user's own turn first makes sure there is a thread token for them, so a
// token that cannot be exchanged fails the request rather than the run.
func (m *Manager) prepare(ctx context.Context, in StartInput, run *Run) error {
	thread, previousModel, err := m.thread(ctx, in)
	if err != nil {
		return err
	}
	if in.ParentID == "" {
		if _, err := m.tokens.Source(ctx, thread.RootID, thread.ControlSetID, in.User.Subject, in.User.Token); err != nil {
			return err
		}
	}
	if err := m.history.SaveThread(ctx, thread); err != nil {
		return err
	}
	run.RootID, run.Connectors, run.ControlSet = thread.RootID, thread.Connectors, thread.ControlSetID
	if previousModel != "" && previousModel != run.Model {
		run.PreviousModel = previousModel
	}
	if err := m.ensureSession(ctx, in.ThreadID, in.AgentID); err != nil {
		return err
	}
	seq, err := m.store.nextSeq(ctx, in.ThreadID)
	if err != nil {
		return err
	}
	switch err := m.history.CreateMessage(ctx, userMessage(thread, in, seq)); {
	case errors.Is(err, opensearch.ErrConflict):
		return fmt.Errorf("%w: message id %s is taken", ErrInvalid, in.MessageID)
	case err != nil:
		return err
	}
	if run.TurnSeq, err = m.store.nextSeq(ctx, in.ThreadID); err != nil {
		return err
	}
	if run.TurnFrom, err = m.log.Last(ctx, in.ThreadID); err != nil {
		return err
	}
	run.SegmentFrom = run.TurnFrom
	if err := m.store.create(ctx, run); err != nil {
		return err
	}
	return m.enqueue(ctx, run, entry(in.User, map[string]string{"message": in.Text}))
}

// claimThread takes ownership of the thread for the new run. It returns the
// existing run when the same message is sent again, waits briefly while the
// previous run is still being prepared or finalised, and otherwise reports
// the thread as busy.
func (m *Manager) claimThread(ctx context.Context, in StartInput, runID string) (*Run, error) {
	deadline := time.Now().Add(finalizeWait)
	for {
		claimed, err := m.store.claimThread(ctx, in.ThreadID, runID)
		if err != nil || claimed {
			return nil, err
		}
		active, err := m.store.active(ctx, in.ThreadID)
		if err != nil {
			return nil, err
		}
		switch {
		case active != nil && active.MessageID == in.MessageID:
			return active, nil
		case active != nil && !active.Status.Terminal(), time.Now().After(deadline):
			return nil, ErrBusy
		}
		select {
		case <-time.After(finalizePoll):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// release gives the thread back once its run is over. Errors are logged, not
// returned: the lease expires on its own.
func (m *Manager) release(ctx context.Context, run *Run) {
	if err := m.store.releaseThread(ctx, run.ThreadID, run.ID); err != nil {
		m.logger.Error().Err(err).Str("run", run.ID).Msg("release thread")
	}
}

// thread is the turn's thread as the turn leaves it; prepare saves it. A new
// conversation's thread (with the request's control set) and a sub-agent's
// thread (under its parent's root, in its control set) are made here; any
// other thread must already exist.
func (m *Manager) thread(ctx context.Context, in StartInput) (thread *opensearch.Thread, previousModel string, err error) {
	now := time.Now()
	thread, err = m.history.Thread(ctx, in.ThreadID)
	switch {
	case errors.Is(err, opensearch.ErrNotFound) && in.newThread:
		caller, _ := opensearch.CallerFrom(ctx)
		if caller.ControlSet == "" {
			return nil, "", fmt.Errorf("%w: a new thread needs a control set", opensearch.ErrNoControlSet)
		}
		thread = &opensearch.Thread{ID: in.ThreadID, RootID: in.ThreadID, WorkspaceID: in.WorkspaceID, CreatedAt: now,
			Labels: opensearch.Labels{ControlSetID: caller.ControlSet}}
	case errors.Is(err, opensearch.ErrNotFound) && in.ParentID != "":
		parent, err := m.history.Thread(ctx, in.ParentID)
		if err != nil {
			return nil, "", fmt.Errorf("parent thread: %w", err)
		}
		thread = &opensearch.Thread{ID: in.ThreadID, ParentID: in.ParentID, RootID: parent.RootID, CreatedAt: now,
			Labels: opensearch.Labels{ControlSetID: parent.ControlSetID}}
	case errors.Is(err, opensearch.ErrNotFound):
		return nil, "", fmt.Errorf("%w: thread %s", ErrNotFound, in.ThreadID)
	case err != nil:
		return nil, "", err
	}
	previousModel = thread.Model
	if in.Connectors != nil {
		thread.Connectors = in.Connectors
	}
	if thread.Title == "" {
		thread.Title = title(in.Text)
	}
	thread.AgentID, thread.Model, thread.UpdatedAt = in.AgentID, in.Model, now
	return thread, previousModel, nil
}

func title(text string) string {
	t := []rune(strings.TrimSpace(strings.SplitN(text, "\n", 2)[0]))
	if len(t) > titleLen {
		t = t[:titleLen]
	}
	return string(t)
}

// ensureSession rebuilds the thread's adk session from history when missing.
func (m *Manager) ensureSession(ctx context.Context, threadID, agentID string) error {
	_, err := m.sessions.Get(ctx, &session.GetRequest{AppName: m.appName, UserID: SessionUser, SessionID: threadID, NumRecentEvents: 1})
	if err == nil {
		return nil
	}
	if !errors.Is(err, session.ErrNotFound) {
		return err
	}
	history, err := m.history.Messages(ctx, threadID, 0, 500)
	if err != nil {
		return err
	}
	return m.runtime.Seed(ctx, threadID, agentID, history)
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
	return &opensearch.Message{ID: in.MessageID, ThreadID: thread.ID, RootID: thread.RootID, Role: role, Seq: seq, CreatedAt: time.Now(), Text: in.Text, Events: raws,
		Labels: opensearch.Labels{ControlSetID: thread.ControlSetID}}
}
