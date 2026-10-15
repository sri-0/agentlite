package agents

import (
	"context"
	"encoding/json"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// seedEvents are the session events of one persisted message.
func (rt *Runtime) seedEvents(ctx context.Context, m opensearch.Message, author string) ([]*session.Event, error) {
	if m.Role == "user" {
		ev := session.NewEvent(ctx, m.ID)
		ev.Author = "user"
		var err error
		ev.Content, _, err = rt.skills.Expand(ctx, m.Text)
		return []*session.Event{ev}, err
	}
	var out []*session.Event
	var model *session.Event
	flush := func() {
		if model != nil && len(model.Content.Parts) > 0 {
			out = append(out, model)
		}
		model = nil
	}
	part := func() *session.Event {
		if model == nil {
			model = session.NewEvent(ctx, m.ID)
			model.Author = author
			model.Content = &genai.Content{Role: genai.RoleModel}
		}
		return model
	}
	args := map[string]string{}
	names := map[string]string{}
	for _, raw := range m.Events {
		ev, err := events.EventFromJSON(raw)
		if err != nil {
			continue
		}
		switch e := ev.(type) {
		case *events.RunStartedEvent:
			if id, _ := e.GetBaseEvent().Metadata[stream.MetaAgentID].(string); id != "" {
				author = id // the agent that answered, not the router that chose it
			}
		case *events.TextMessageContentEvent:
			part().Content.Parts = append(part().Content.Parts, genai.NewPartFromText(e.Delta))
		case *events.ToolCallStartEvent:
			names[e.ToolCallID] = e.ToolCallName
		case *events.ToolCallArgsEvent:
			args[e.ToolCallID] += e.Delta
		case *events.ToolCallEndEvent:
			var a map[string]any
			_ = json.Unmarshal([]byte(args[e.ToolCallID]), &a)
			p := genai.NewPartFromFunctionCall(names[e.ToolCallID], a)
			p.FunctionCall.ID = e.ToolCallID
			part().Content.Parts = append(part().Content.Parts, p)
		case *events.ToolCallResultEvent:
			flush()
			var r map[string]any
			_ = json.Unmarshal([]byte(e.Content), &r)
			resp := session.NewEvent(ctx, m.ID)
			resp.Author = author
			resp.Content = genai.NewContentFromFunctionResponse(names[e.ToolCallID], r, genai.RoleUser)
			resp.Content.Parts[0].FunctionResponse.ID = e.ToolCallID
			out = append(out, resp)
		}
	}
	flush()
	return out, nil
}
