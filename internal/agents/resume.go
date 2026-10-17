package agents

import (
	"context"
	"fmt"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"agentlite/internal/runs"
	"agentlite/internal/stream"
	"agentlite/internal/tools/builtin"
)

// content is the user message, with what its references (/name) resolve to
// and the activities that show them; or, on resume, the
// adk_request_confirmation responses adk expects for each answered interrupt,
// with the CUSTOM approval events that record the answers.
func (rt *Runtime) content(ctx context.Context, threadID string, in runs.Input) (*genai.Content, []stream.Event, error) {
	if len(in.Resume) == 0 {
		return rt.refs.Expand(ctx, in.Run.TurnID, in.Message)
	}
	got, err := rt.sessions.Get(ctx, &session.GetRequest{AppName: rt.appName, UserID: runs.SessionUser, SessionID: threadID})
	if err != nil {
		return nil, nil, err
	}
	c := &genai.Content{Role: genai.RoleUser}
	var recorded []stream.Event
	for _, entry := range in.Resume {
		call, ok := pendingCall(got.Session, entry.InterruptID)
		if !ok {
			return nil, nil, fmt.Errorf("no pending call for interrupt %q", entry.InterruptID)
		}
		original, err := toolconfirmation.OriginalCallFrom(call)
		if err != nil {
			return nil, nil, fmt.Errorf("interrupt %q: %w", entry.InterruptID, err)
		}
		payload, _ := entry.Payload.(map[string]any)
		approved, _ := payload["approved"].(bool)
		justification, _ := payload["justification"].(string)
		// question takes its answer as its result, so adk must call it again.
		confirmed := approved || original.Name == builtin.Question
		c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID: entry.InterruptID, Name: call.Name, Response: map[string]any{"confirmed": confirmed, "payload": payload},
		}})
		recorded = append(recorded, events.NewCustomEvent(stream.CustomApproval, events.WithValue(approval{ToolCallID: original.ID, Approved: confirmed, Justification: justification})))
	}
	return c, recorded, nil
}

// approval is the recorded decision for a confirmation, keyed by the real call.
type approval struct {
	ToolCallID    string `json:"toolCallId"`
	Approved      bool   `json:"approved"`
	Justification string `json:"justification,omitempty"`
}

// pendingCall finds the function call an interrupt id refers to.
func pendingCall(sess session.Session, callID string) (*genai.FunctionCall, bool) {
	evs := sess.Events()
	for i := evs.Len() - 1; i >= 0; i-- {
		ev := evs.At(i)
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.ID == callID {
				return p.FunctionCall, true
			}
		}
	}
	return nil, false
}
