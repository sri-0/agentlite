// Package ws is the agent chat socket: one WebSocket per client carrying any
// number of thread subscriptions. Runs are started, answered and cancelled
// over REST; the socket only streams. The payload of every event frame is an
// AG-UI 1.0 event, untouched; the envelope adds only what AG-UI's per-run
// transport has no place for: the thread (one socket, many threads) and the
// log cursor (reconnect).
package ws

import "encoding/json"

// Version is the protocol version the client must send as ?v=.
const Version = "1"

// Client frame types.
const (
	FrameSubscribe   = "subscribe"
	FrameUnsubscribe = "unsubscribe"
)

// Server frame types.
const (
	FrameEvent = "event"
	FrameIdle  = "idle"
	FrameError = "error"
)

// ClientFrame is a request from the client. Every frame names its thread.
type ClientFrame struct {
	Type     string `json:"type"`
	ThreadID string `json:"threadId"`
	// subscribe: log cursor to continue from; empty replays the active turn.
	After string `json:"after,omitempty"`
}

// ServerFrame is an event with its thread and cursor, an idle notice, or an error.
type ServerFrame struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"threadId"`
	Seq      string          `json:"seq,omitempty"`
	Event    json.RawMessage `json:"event,omitempty"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// Error codes.
const (
	CodeBadRequest = "bad_request"
	CodeInternal   = "internal"
)
