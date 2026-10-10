package stream

import "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"

// Coalesce merges contiguous deltas of the same text, reasoning or tool-call
// block into one event before storage. Only neighbours inside a start/end
// pair are merged, so the result is a valid event sequence that folds to the
// same state. (Context compaction of a thread is adk's job; see agents.)
func Coalesce(evs []Event) []Event {
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		if n := len(out); n > 0 && mergeInto(out[n-1], ev) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func mergeInto(prev, next Event) bool {
	switch p := prev.(type) {
	case *events.TextMessageContentEvent:
		if n, ok := next.(*events.TextMessageContentEvent); ok && n.MessageID == p.MessageID {
			p.Delta += n.Delta
			return true
		}
	case *events.ReasoningMessageContentEvent:
		if n, ok := next.(*events.ReasoningMessageContentEvent); ok && n.MessageID == p.MessageID {
			p.Delta += n.Delta
			return true
		}
	case *events.ToolCallArgsEvent:
		if n, ok := next.(*events.ToolCallArgsEvent); ok && n.ToolCallID == p.ToolCallID {
			p.Delta += n.Delta
			return true
		}
	}
	return false
}
