// Package agent is the run lifecycle: starting, executing, finalising and
// subscribing to runs. Everything crosses between its parts as data in Valkey,
// so the worker loop can later move to another process unchanged.
package agent

import (
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

// Run is one assistant turn. It is stored as a Valkey hash. A turn may span
// several segments when it pauses for input; the log cursors mark where the
// turn and the current segment begin.
type Run struct {
	ID          string
	ThreadID    string
	RootID      string
	AgentID     string
	Model       string
	MessageID   string // the user message that started the turn
	TurnID      string // the assistant message id
	TurnSeq     int64  // the assistant message's position in the thread
	TurnFrom    string // log cursor before the turn's first event
	SegmentFrom string // log cursor before the current segment's first event
	Status      Status
	Result      string // final text, capped
	Error       string
	StartedAt   time.Time
	UpdatedAt   time.Time
}

const resultCap = 64 << 10

func (r *Run) fields() map[string]string {
	return map[string]string{
		"id": r.ID, "thread_id": r.ThreadID, "root_id": r.RootID, "agent_id": r.AgentID, "model": r.Model,
		"message_id": r.MessageID, "turn_id": r.TurnID, "turn_seq": strconv.FormatInt(r.TurnSeq, 10),
		"turn_from": r.TurnFrom, "segment_from": r.SegmentFrom, "status": string(r.Status),
		"result": r.Result, "error": r.Error,
		"started_at": strconv.FormatInt(r.StartedAt.UnixMilli(), 10),
		"updated_at": strconv.FormatInt(r.UpdatedAt.UnixMilli(), 10),
	}
}

func runFromFields(f map[string]string) *Run {
	n := func(k string) int64 {
		v, _ := strconv.ParseInt(f[k], 10, 64)
		return v
	}
	return &Run{
		ID: f["id"], ThreadID: f["thread_id"], RootID: f["root_id"], AgentID: f["agent_id"], Model: f["model"],
		MessageID: f["message_id"], TurnID: f["turn_id"], TurnSeq: n("turn_seq"),
		TurnFrom: f["turn_from"], SegmentFrom: f["segment_from"], Status: Status(f["status"]),
		Result: f["result"], Error: f["error"],
		StartedAt: time.UnixMilli(n("started_at")), UpdatedAt: time.UnixMilli(n("updated_at")),
	}
}
