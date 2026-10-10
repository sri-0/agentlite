// Package hitl keeps the human-in-the-loop records: every interrupt a run
// pauses on, what was asked, and how the user decided. adk holds the pending
// state inside the session and resumes the run; this package is the durable,
// queryable record around it, stored in Valkey.
package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/stream"
)

// Interrupt statuses.
const (
	StatusPending   = "pending"
	StatusResolved  = "resolved"
	StatusCancelled = "cancelled"
)

// Interrupt is one pause of a run waiting for the user.
type Interrupt struct {
	ID        string         `json:"id"`
	ThreadID  string         `json:"thread_id"`
	RunID     string         `json:"run_id"`
	Reason    string         `json:"reason"` // confirmation | input_required
	ToolCall  string         `json:"tool_call_id"`
	ToolName  string         `json:"tool_name"`
	Args      map[string]any `json:"args,omitempty"`
	Message   string         `json:"message,omitempty"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`

	// Set when decided.
	Decision  *Decision `json:"decision,omitempty"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

// Decision is the user's answer: approval of a tool call, or the output of a
// client-answered tool.
type Decision struct {
	Approved      *bool  `json:"approved,omitempty"`
	Justification string `json:"justification,omitempty"`
	Output        any    `json:"output,omitempty"`
}

// ResumeEntry is the AG-UI form of a decision for the run engine.
func (i *Interrupt) ResumeEntry() types.ResumeEntry {
	payload := any(i.Decision.Output)
	if i.Reason == stream.ReasonConfirmation {
		payload = map[string]any{"approved": i.Decision.Approved != nil && *i.Decision.Approved, "justification": i.Decision.Justification}
	}
	return types.ResumeEntry{InterruptID: i.ID, Status: types.ResumeStatusResolved, Payload: payload}
}

// Errors for a missing interrupt and for one already decided.
var (
	ErrNotFound   = errors.New("interrupt not found")
	ErrNotPending = errors.New("interrupt is not pending")
)

const retention = 7 * 24 * time.Hour

// Store keeps interrupts in Valkey: one hash per interrupt and a set of
// pending ids per thread.
type Store struct{ db valkey.Client }

// NewStore wraps a client.
func NewStore(db valkey.Client) *Store { return &Store{db: db} }

func key(id string) string              { return "hitl:" + id }
func pendingKey(threadID string) string { return "hitl:thread:" + threadID + ":pending" }

// Add records the interrupts a run just paused on, from the AG-UI interrupts
// the encoder produced.
func (s *Store) Add(ctx context.Context, threadID, runID string, interrupts []types.Interrupt) error {
	for _, in := range interrupts {
		rec := Interrupt{
			ID: in.ID, ThreadID: threadID, RunID: runID, Reason: in.Reason, ToolCall: in.ToolCallID,
			Message: in.Message, Status: StatusPending, CreatedAt: time.Now(),
		}
		rec.ToolName, _ = in.Metadata["toolName"].(string)
		rec.Args, _ = in.Metadata["args"].(map[string]any)
		if err := s.save(ctx, &rec); err != nil {
			return err
		}
		if err := s.db.Do(ctx, s.db.B().Sadd().Key(pendingKey(threadID)).Member(in.ID).Build()).Error(); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists a thread's open interrupts.
func (s *Store) Pending(ctx context.Context, threadID string) ([]Interrupt, error) {
	ids, err := s.db.Do(ctx, s.db.B().Smembers().Key(pendingKey(threadID)).Build()).AsStrSlice()
	if err != nil {
		return nil, err
	}
	out := make([]Interrupt, 0, len(ids))
	for _, id := range ids {
		rec, err := s.Get(ctx, id)
		if err != nil {
			continue // expired
		}
		out = append(out, *rec)
	}
	return out, nil
}

// Get fetches one interrupt.
func (s *Store) Get(ctx context.Context, id string) (*Interrupt, error) {
	raw, err := s.db.Do(ctx, s.db.B().Get().Key(key(id)).Build()).ToString()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, err
	}
	var rec Interrupt
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Decide records the user's answer to a pending interrupt and returns it.
func (s *Store) Decide(ctx context.Context, id string, d Decision) (*Interrupt, error) {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.Status != StatusPending {
		return nil, fmt.Errorf("interrupt %s: %w", id, ErrNotPending)
	}
	rec.Status, rec.Decision, rec.DecidedAt = StatusResolved, &d, time.Now()
	if err := s.save(ctx, rec); err != nil {
		return nil, err
	}
	return rec, s.db.Do(ctx, s.db.B().Srem().Key(pendingKey(rec.ThreadID)).Member(id).Build()).Error()
}

// Cancel closes every pending interrupt of a thread.
func (s *Store) Cancel(ctx context.Context, threadID string) error {
	pending, err := s.Pending(ctx, threadID)
	if err != nil {
		return err
	}
	for i := range pending {
		pending[i].Status, pending[i].DecidedAt = StatusCancelled, time.Now()
		if err := s.save(ctx, &pending[i]); err != nil {
			return err
		}
	}
	return s.db.Do(ctx, s.db.B().Del().Key(pendingKey(threadID)).Build()).Error()
}

func (s *Store) save(ctx context.Context, rec *Interrupt) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.db.Do(ctx, s.db.B().Set().Key(key(rec.ID)).Value(string(raw)).Ex(retention).Build()).Error()
}
