package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// seed rebuilds a thread's adk session from its persisted messages: user text
// becomes user content, assistant text and tool calls become model content,
// tool results become function responses. Reasoning and activities are dropped.
func seed(ctx context.Context, svc session.Service, app, user, threadID, author string, msgs []opensearch.Message) error {
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: app, UserID: user, SessionID: threadID})
	if err != nil {
		return err
	}
	for _, m := range msgs {
		for _, ev := range seedEvents(ctx, m, author) {
			if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
				return fmt.Errorf("seed %s: %w", m.ID, err)
			}
		}
	}
	return nil
}

func seedEvents(ctx context.Context, m opensearch.Message, author string) []*session.Event {
	if m.Role == "user" {
		ev := session.NewEvent(ctx, m.ID)
		ev.Author = "user"
		ev.Content = genai.NewContentFromText(m.Text, genai.RoleUser)
		return []*session.Event{ev}
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
	return out
}
