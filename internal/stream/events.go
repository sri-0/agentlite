// Package stream is the wire and storage format: AG-UI 1.0 events. It encodes
// adk session events into AG-UI events and coalesces them for storage. The
// AG-UI Go SDK supplies the types; this package adds the conventions agentlite
// uses.
package stream

import (
	"strings"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"google.golang.org/genai"
)

// Event is one AG-UI event.
type Event = events.Event

// Activity types carried in ACTIVITY_SNAPSHOT events, reconciled by messageId.
const (
	ActivityTaskList   = "task_list"  // messageId "tasks"
	ActivityArtifact   = "artifact"   // messageId = artifact id
	ActivityAgent      = "agent"      // messageId = subagentRunId
	ActivityCompaction = "compaction" // messageId = "compaction:<uuid>"
	ActivityModel      = "model"      // messageId = "model:<turn id>": the turn changed the thread's model
	ActivitySkill      = "skill"      // messageId = "skill:<turn id>": the turn's message invoked a skill (/name)
)

// Metadata keys: the turn on RUN_STARTED and RUN_FINISHED, a tool's UI
// renderer on TOOL_CALL_START.
const (
	MetaTurnID     = "turnId"
	MetaAgentID    = "agentId"
	MetaModel      = "model"
	MetaCreatedAt  = "createdAt"
	MetaDurationMs = "durationMs"
	MetaUI         = "ui"
)

// ReasonConfirmation is the reason of every interrupt: adk's tool
// confirmation, answered with an approval or, for a question, an output.
const ReasonConfirmation = "confirmation"

// CustomApproval is the custom event carrying the user's decision on a confirmation.
const CustomApproval = "approval"

// OutcomeCancelled marks a RUN_FINISHED of a cancelled run; the SDK defines
// only success and interrupt.
const OutcomeCancelled = events.RunFinishedOutcomeType("cancelled")

// TextOf joins a content's plain text parts; tool calls and thoughts are skipped.
func TextOf(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Terminal reports whether the event ends a run segment.
func Terminal(e Event) bool {
	t := e.Type()
	return t == events.EventTypeRunFinished || t == events.EventTypeRunError
}
