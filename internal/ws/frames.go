// Package ws is the agent chat socket: one WebSocket per client, carrying
// everything between the client and its agents. The client starts runs,
// answers interrupts, cancels and follows threads with frames; the server
// streams each followed thread's AG-UI events, untouched, inside an envelope
// that adds only what AG-UI's per-run transport has no place for: the thread
// (one socket, many threads) and the log cursor (reconnect).
package ws

import (
	"encoding/json"

	"agentlite/internal/hitl"
)

// Version is the protocol version the client must send as ?v=.
const Version = "1"

// Client frame types.
const (
	FrameRun         = "run"         // start a turn (a new thread when threadId is empty)
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

	// run: the chat-completions shape; the last message is the user's turn,
	// and its id makes a retry idempotent.
	Model      string    `json:"model,omitempty"`
	AgentID    string    `json:"agentId,omitempty"`
	Connectors *[]string `json:"connectors,omitempty"`
	Messages   []Message `json:"messages,omitempty"`

	// answer: the interrupt and the decision ({approved, justification} or {output}).
	InterruptID string `json:"interruptId,omitempty"`
	hitl.Decision

	// subscribe: log cursor to continue from; empty replays the active turn.
	After string `json:"after,omitempty"`
}

// Message is one chat message of a run frame.
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
