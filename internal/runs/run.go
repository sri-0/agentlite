// Package runs is the run lifecycle: starting, executing, finalising and
// subscribing to runs. Everything crosses between its parts as data in
// Valkey, which is what will let the worker loop move to another process.
package runs

import (
	"encoding/json"
	"strconv"
	"time"
)

// Status of a run.
type Status string

// Run statuses.
const (
	StatusQueued        Status = "queued"
	StatusRunning       Status = "running"
	StatusAwaitingInput Status = "awaiting_input"
	StatusDone          Status = "done"
	StatusError         Status = "error"
	StatusCancelled     Status = "cancelled"
)

// Terminal reports whether the run has ended.
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusError || s == StatusCancelled
}

// Run is one assistant turn. It is stored as a Valkey hash, its status as a
// key of its own (store.transition). A turn may span several segments when
// it pauses for input; the log cursors mark where the turn and the current
// segment begin.
type Run struct {
	ID       string
	ThreadID string
	RootID   string
	AgentID  string
	Model    string
	// PreviousModel is the model of the thread's previous turn, set only when
	// it differs from Model: the turn starts with a model change marker.
	PreviousModel string
	MessageID     string    // the user message that started the turn
	TurnID        string    // the assistant message id
	TurnSeq       int64     // the assistant message's position in the thread
	TurnFrom      string    // log cursor before the turn's first event
	SegmentFrom   string    // log cursor before the current segment's first event
	Connectors    *[]string // the thread's enabled connectors at start; nil means all
	Status        Status
	Result        string // final text, capped
	Error         string
	StartedAt     time.Time
}

const resultCap = 64 << 10

// Root reports whether the run was started by a user rather than by a
// parent run's subagent tool.
func (r *Run) Root() bool { return r.RootID == r.ThreadID }

func (r *Run) fields() map[string]string {
	f := map[string]string{
		"id": r.ID, "thread_id": r.ThreadID, "root_id": r.RootID, "agent_id": r.AgentID, "model": r.Model,
		"previous_model": r.PreviousModel,
		"message_id":     r.MessageID, "turn_id": r.TurnID, "turn_seq": strconv.FormatInt(r.TurnSeq, 10),
		"turn_from": r.TurnFrom, "segment_from": r.SegmentFrom,
		"result": r.Result, "error": r.Error,
		"started_at": strconv.FormatInt(r.StartedAt.UnixMilli(), 10),
	}
	if r.Connectors != nil {
		raw, _ := json.Marshal(r.Connectors)
		f["connectors"] = string(raw)
	}
	return f
}

func runFromFields(f map[string]string) *Run {
	n := func(k string) int64 {
		v, _ := strconv.ParseInt(f[k], 10, 64)
		return v
	}
	r := &Run{
		ID: f["id"], ThreadID: f["thread_id"], RootID: f["root_id"], AgentID: f["agent_id"], Model: f["model"],
		PreviousModel: f["previous_model"],
		MessageID:     f["message_id"], TurnID: f["turn_id"], TurnSeq: n("turn_seq"),
		TurnFrom: f["turn_from"], SegmentFrom: f["segment_from"],
		Result: f["result"], Error: f["error"],
		StartedAt: time.UnixMilli(n("started_at")),
	}
	if raw, ok := f["connectors"]; ok {
		_ = json.Unmarshal([]byte(raw), &r.Connectors)
	}
	return r
}
