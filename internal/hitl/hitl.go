// Package hitl keeps the human-in-the-loop records: every interrupt a run
// pauses on, what was asked, and how the user decided. adk holds the pending
// state inside the session and resumes the run; this package is the durable,
// queryable record around it, stored in Valkey.
package hitl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	Reason    string         `json:"reason"` // confirmation
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

// ResumeEntry is the AG-UI form of a decision for the run engine. A record
// without a decision is refused: there is nothing to resume with.
func (i *Interrupt) ResumeEntry() (types.ResumeEntry, error) {
	if i.Decision == nil {
		return types.ResumeEntry{}, fmt.Errorf("interrupt %s has no decision", i.ID)
	}
	// An output answers a question.
	approved := i.Decision.Output != nil || i.Decision.Approved != nil && *i.Decision.Approved
	payload := map[string]any{"approved": approved, "justification": i.Decision.Justification, "output": i.Decision.Output}
	return types.ResumeEntry{InterruptID: i.ID, Status: types.ResumeStatusResolved, Payload: payload}, nil
}

// Errors for a missing interrupt and for one already decided.
var (
	ErrNotFound   = errors.New("interrupt not found")
	ErrNotPending = errors.New("interrupt is not pending")
)

const retention = 7 * 24 * time.Hour

// Store keeps interrupts in Valkey: one JSON record per interrupt and a set
// of pending ids per thread. A record leaves pending once, by answer or by
// cancel: each is a compare-and-set on the record as it was read.
type Store struct{ db valkey.Client }

// NewStore wraps a client.
func NewStore(db valkey.Client) *Store { return &Store{db: db} }

func key(id string) string              { return "hitl:" + id }
func pendingKey(threadID string) string { return "hitl:thread:" + threadID + ":pending" }
func decidedKey(threadID string) string { return "hitl:thread:" + threadID + ":decided" }

// Add records the interrupts a run just paused on, from the AG-UI interrupts
// the encoder produced.
func (s *Store) Add(ctx context.Context, threadID, runID string, interrupts []types.Interrupt) error {
	var cmds valkey.Commands
	for _, in := range interrupts {
		rec := Interrupt{
			ID: in.ID, ThreadID: threadID, RunID: runID, Reason: in.Reason, ToolCall: in.ToolCallID,
			Message: in.Message, Status: StatusPending, CreatedAt: time.Now(),
		}
		rec.ToolName, _ = in.Metadata[stream.MetaToolName].(string)
		rec.Args, _ = in.Metadata[stream.MetaArgs].(map[string]any)
		if request, ok := in.Metadata[stream.MetaRequest].(map[string]any); ok && request[stream.MetaToolName] == "question" {
			rec.Args, _ = request[stream.MetaArgs].(map[string]any) // the questions
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		cmds = append(cmds, s.db.B().Set().Key(key(rec.ID)).Value(string(raw)).Ex(retention).Build(),
			s.db.B().Sadd().Key(pendingKey(threadID)).Member(rec.ID).Build())
	}
	for _, r := range s.db.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return err
		}
	}
	return nil
}

// Pending lists a thread's open interrupts, oldest first.
func (s *Store) Pending(ctx context.Context, threadID string) ([]Interrupt, error) {
	out, _, err := s.members(ctx, pendingKey(threadID))
	slices.SortFunc(out, func(a, b Interrupt) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, err
}

// Get fetches one interrupt.
func (s *Store) Get(ctx context.Context, id string) (*Interrupt, error) {
	rec, _, err := s.get(ctx, id)
	return rec, err
}

// get returns the record and its JSON as stored, for a compare-and-set.
func (s *Store) get(ctx context.Context, id string) (*Interrupt, string, error) {
	raw, err := s.db.Do(ctx, s.db.B().Get().Key(key(id)).Build()).ToString()
	if valkey.IsValkeyNil(err) {
		return nil, "", fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, "", err
	}
	var rec Interrupt
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, "", err
	}
	return &rec, raw, nil
}

// members reads the records of a set's ids with one MGET; an expired record
// is skipped. It also returns each record's JSON as stored.
func (s *Store) members(ctx context.Context, set string) ([]Interrupt, []string, error) {
	ids, err := s.db.Do(ctx, s.db.B().Smembers().Key(set).Build()).AsStrSlice()
	if err != nil || len(ids) == 0 {
		return nil, nil, err
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = key(id)
	}
	vals, err := s.db.Do(ctx, s.db.B().Mget().Key(keys...).Build()).ToArray()
	if err != nil {
		return nil, nil, err
	}
	var recs []Interrupt
	var raws []string
	for _, v := range vals {
		raw, err := v.ToString()
		if valkey.IsValkeyNil(err) {
			continue // expired
		}
		if err != nil {
			return nil, nil, err
		}
		var rec Interrupt
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, nil, err
		}
		recs, raws = append(recs, rec), append(raws, raw)
	}
	return recs, raws, nil
}

// replace stores rec over the record only if it is still raw; false when
// another answer or a cancel changed it first.
func (s *Store) replace(ctx context.Context, rec *Interrupt, raw string) (bool, error) {
	next, err := json.Marshal(rec)
	if err != nil {
		return false, err
	}
	err = s.db.Do(ctx, s.db.B().Set().Key(key(rec.ID)).Value(string(next)).Ifeq(raw).Ex(retention).Build()).Error()
	if valkey.IsValkeyNil(err) {
		return false, nil
	}
	return err == nil, err
}

// Decide records the answer to one of a thread's pending interrupts and
// reports how many are still waiting. An interrupt is answered once; a second
// answer, or one after a cancel, gets ErrNotPending. Each answer joins the
// decided set before it leaves the pending one, so whoever sees none left
// finds every answer decided.
func (s *Store) Decide(ctx context.Context, threadID, id string, d Decision) (int64, error) {
	rec, raw, err := s.get(ctx, id)
	if err == nil && rec.ThreadID != threadID {
		err = fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return 0, err
	}
	if rec.Status != StatusPending {
		return 0, fmt.Errorf("interrupt %s: %w", id, ErrNotPending)
	}
	rec.Status, rec.Decision, rec.DecidedAt = StatusResolved, &d, time.Now()
	if ok, err := s.replace(ctx, rec, raw); !ok || err != nil {
		return 0, cmp.Or(err, fmt.Errorf("interrupt %s: %w", id, ErrNotPending))
	}
	res := s.db.DoMulti(ctx,
		s.db.B().Sadd().Key(decidedKey(threadID)).Member(id).Build(),
		s.db.B().Srem().Key(pendingKey(threadID)).Member(id).Build(),
		s.db.B().Scard().Key(pendingKey(threadID)).Build())
	for _, r := range res[:2] {
		if err := r.Error(); err != nil {
			return 0, err
		}
	}
	return res[2].AsInt64()
}

// TakeDecided returns the thread's answered interrupts that have not been
// handed to the run yet, and forgets them.
func (s *Store) TakeDecided(ctx context.Context, threadID string) ([]Interrupt, error) {
	out, _, err := s.members(ctx, decidedKey(threadID))
	if err != nil {
		return nil, err
	}
	return out, s.db.Do(ctx, s.db.B().Del().Key(decidedKey(threadID)).Build()).Error()
}

// Cancel closes every pending interrupt of a thread; one answered at the
// same moment keeps its answer.
func (s *Store) Cancel(ctx context.Context, threadID string) error {
	recs, raws, err := s.members(ctx, pendingKey(threadID))
	if err != nil {
		return err
	}
	for i := range recs {
		recs[i].Status, recs[i].DecidedAt = StatusCancelled, time.Now()
		if _, err := s.replace(ctx, &recs[i], raws[i]); err != nil {
			return err
		}
	}
	return s.db.Do(ctx, s.db.B().Del().Key(pendingKey(threadID), decidedKey(threadID)).Build()).Error()
}
