// Package stream is the wire and storage format: AG-UI 1.0 events. It encodes
// adk session events into AG-UI events and compacts them for storage. The AG-UI
// Go SDK supplies the types; this package adds the conventions agentlite uses.
package stream

import (
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
)

// Event is one AG-UI event.
type Event = events.Event

// Activity types carried in ACTIVITY_SNAPSHOT events, reconciled by messageId.
const (
	ActivityTaskList = "task_list" // messageId "tasks"
	ActivityArtifact = "artifact"  // messageId = artifact id
	ActivityAgent    = "agent"     // messageId = subagentRunId
)

// Metadata keys on RUN_STARTED and RUN_FINISHED.
const (
	MetaTurnID     = "turnId"
	MetaAgentID    = "agentId"
	MetaModel      = "model"
	MetaCreatedAt  = "createdAt"
	MetaDurationMs = "durationMs"
)

// Interrupt reasons.
const (
	ReasonConfirmation  = "confirmation"   // approval of a tool call
	ReasonInputRequired = "input_required" // a client-answered tool
)

// Custom event names.
const CustomApproval = "approval"

// Marshal serialises an event for the log and for storage.
func Marshal(e Event) ([]byte, error) { return e.ToJSON() }

// Unmarshal parses an event from the log or from storage.
func Unmarshal(b []byte) (Event, error) { return events.EventFromJSON(b) }

// Terminal reports whether the event ends a run segment.
func Terminal(e Event) bool {
	t := e.Type()
	return t == events.EventTypeRunFinished || t == events.EventTypeRunError
}
