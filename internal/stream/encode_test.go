package stream

import (
	"context"
	"errors"
	"testing"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// modelEvent is an adk event of the assistant agent.
func modelEvent(partial bool, parts ...*genai.Part) *session.Event {
	ev := session.NewEvent(context.Background(), "inv")
	ev.Author, ev.Partial = "assistant", partial
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
	return ev
}

func thought(s string) *genai.Part { return &genai.Part{Text: s, Thought: true} }

// TestEncoderSequence: what the encoder makes of adk's events, and what
// Coalesce keeps of it for storage, are valid AG-UI sequences by the
// upstream SDK's own validator.
func TestEncoderSequence(t *testing.T) {
	result := session.NewEvent(context.Background(), "inv")
	result.Author = "assistant"
	result.Content = genai.NewContentFromFunctionResponse("calculate", map[string]any{"result": 2}, genai.RoleUser)
	result.Content.Parts[0].FunctionResponse.ID = "c1"
	call := genai.NewPartFromFunctionCall("calculate", map[string]any{"expression": "1+1"})
	call.FunctionCall.ID = "c1"
	ask := genai.NewPartFromFunctionCall(toolconfirmation.FunctionCallName, map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "q1", Name: "question", Args: map[string]any{"questions": []any{}}},
		"toolConfirmation":     map[string]any{"hint": "Answer the questions."},
	})
	ask.FunctionCall.ID = "confirm-q1"

	tests := []struct {
		name   string
		in     []*session.Event
		end    func(*Encoder)
		paused bool
	}{
		{name: "streamed text", in: []*session.Event{
			modelEvent(true, genai.NewPartFromText("Hel")), modelEvent(true, genai.NewPartFromText("lo")),
			modelEvent(false, genai.NewPartFromText("Hello")),
		}},
		{name: "reasoning, then text", in: []*session.Event{
			modelEvent(true, thought("think")), modelEvent(true, genai.NewPartFromText("ok")),
			modelEvent(false, thought("think"), genai.NewPartFromText("ok")),
		}},
		{name: "tool call and result", in: []*session.Event{
			modelEvent(false, genai.NewPartFromText("Let me see."), call), result, modelEvent(false, genai.NewPartFromText("2")),
		}},
		{name: "pause on a question", in: []*session.Event{modelEvent(false, ask)}, paused: true},
		{name: "failure mid-text", in: []*session.Event{modelEvent(true, genai.NewPartFromText("par"))},
			end: func(e *Encoder) { e.Fail(errors.New("upstream closed")) }},
		{name: "cancel mid-text", in: []*session.Event{modelEvent(true, genai.NewPartFromText("par"))},
			end: func(e *Encoder) { e.Cancel() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out []Event
			enc := NewEncoder("thread", "run", "model", func(e Event) { out = append(out, e) })
			enc.Start("turn", "assistant")
			for _, ev := range tt.in {
				enc.Handle(ev)
			}
			if tt.end != nil {
				tt.end(enc)
			} else if interrupts := enc.Finish(); (len(interrupts) > 0) != tt.paused {
				t.Fatalf("interrupts %v, want paused=%v", interrupts, tt.paused)
			}
			if err := events.ValidateSequence(out); err != nil {
				t.Errorf("encoded: %v", err)
			}
			if err := events.ValidateSequence(Coalesce(out)); err != nil {
				t.Errorf("coalesced: %v", err)
			}
		})
	}
}
