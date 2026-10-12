// Package ws is the agent chat socket: one WebSocket per client, carrying
// everything between the client and its agents on threads that exist. The
// client follows threads, sends turns, answers interrupts and cancels with
// frames; the server streams each followed thread's AG-UI events, untouched,
// inside an envelope that adds only what AG-UI's per-run transport has no
// place for: the thread (one socket, many threads) and the log cursor
// (reconnect). A new thread is created over HTTP (POST /v1/agent/chat).
package ws

import (
	"encoding/json"
	"fmt"

	"agentlite/internal/hitl"
	"agentlite/internal/runs"
)

// Version is the protocol version the client must send as ?v=.
const Version = "1"

// Client frame types.
const (
	FrameRun         = "run"         // start a turn on the thread
	FrameAnswer      = "answer"      // answer one of a paused run's interrupts
	FrameCancel      = "cancel"      // stop the thread's run
	FrameSubscribe   = "subscribe"   // follow a thread from a cursor
	FrameUnsubscribe = "unsubscribe" // stop following it
)

// Server frame types.
const (
	FrameAck   = "ack"   // a run, answer or cancel was accepted
	FrameEvent = "event" // an AG-UI event of a followed thread
	FrameIdle  = "idle"  // the followed thread has no active run
	FrameError = "error"
)

// ClientFrame is a request from the client. RequestID, when set, is echoed on
// the ack or error it causes.
type ClientFrame struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId,omitempty"`
	ThreadID  string `json:"threadId,omitempty"`

	// run: the turn.
	Turn

	// answer: the interrupt and the decision ({approved, justification} or {output}).
	InterruptID string `json:"interruptId,omitempty"`
	hitl.Decision

	// subscribe: log cursor to continue from; empty replays the active turn.
	After string `json:"after,omitempty"`
}

// Turn is a user's turn, sent in a run frame or in the request that creates a
// thread. It has the chat-completions shape: the last message is the user's,
// and its id makes a retry idempotent.
type Turn struct {
	Model      string    `json:"model,omitempty"`
	AgentID    string    `json:"agentId,omitempty"`
	Connectors *[]string `json:"connectors,omitempty"`
	Messages   []Message `json:"messages,omitempty"`
}

// StartInput is the run the turn starts on the thread; an empty thread id
// starts a new thread.
func (t Turn) StartInput(threadID string) (runs.StartInput, error) {
	if len(t.Messages) == 0 || t.Messages[len(t.Messages)-1].Role != "user" {
		return runs.StartInput{}, fmt.Errorf("%w: the last message must be from the user", runs.ErrInvalid)
	}
	last := t.Messages[len(t.Messages)-1]
	return runs.StartInput{
		ThreadID: threadID, AgentID: t.AgentID, Model: t.Model, Connectors: t.Connectors, MessageID: last.ID, Text: last.Content,
	}, nil
}

// Message is one chat message of a turn.
type Message struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ServerFrame is an ack, an event with its thread and cursor, an idle notice,
// or an error. An ack's Seq is the cursor to subscribe from.
type ServerFrame struct {
	Type      string          `json:"type"`
	RequestID string          `json:"requestId,omitempty"`
	ThreadID  string          `json:"threadId,omitempty"`
	RunID     string          `json:"runId,omitempty"`
	Seq       string          `json:"seq,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	Code      string          `json:"code,omitempty"`
	Message   string          `json:"message,omitempty"`
}

// Error codes.
const (
	CodeBadRequest = "bad_request"
	CodeNotFound   = "not_found"
	CodeBusy       = "busy"
	CodeNotPaused  = "not_paused"
	CodeInternal   = "internal"
)
