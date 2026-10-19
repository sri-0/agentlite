package agents

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
)

// TestSeedRoundTrip: a turn encoded, coalesced and stored as JSON, as the
// finalizer stores it, seeds the same text, tool call and tool result back
// into a session, so a thread whose session expired goes on where it was.
func TestSeedRoundTrip(t *testing.T) {
	ctx := context.Background()
	call := genai.NewPartFromFunctionCall("calculate", map[string]any{"expression": "1+1"})
	call.FunctionCall.ID = "c1"
	answer := session.NewEvent(ctx, "inv")
	answer.Author, answer.Content = "assistant", &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromText("Let me see."), call}}
	result := session.NewEvent(ctx, "inv")
	result.Author = "assistant"
	result.Content = genai.NewContentFromFunctionResponse("calculate", map[string]any{"result": float64(2)}, genai.RoleUser)
	result.Content.Parts[0].FunctionResponse.ID = "c1"
	final := session.NewEvent(ctx, "inv")
	final.Author, final.Content = "assistant", genai.NewContentFromText("It is 2.", genai.RoleModel)

	var out []stream.Event
	enc := stream.NewEncoder("thread", "run", "model", func(e stream.Event) { out = append(out, e) })
	enc.Start("turn", "assistant")
	for _, ev := range []*session.Event{answer, result, final} {
		enc.Handle(ev)
	}
	enc.Finish()
	msg := opensearch.Message{ID: "turn", Role: "assistant"}
	for _, e := range stream.Coalesce(out) {
		raw, err := e.ToJSON()
		if err != nil {
			t.Fatal(err)
		}
		msg.Events = append(msg.Events, json.RawMessage(raw))
	}

	var got []*genai.Content
	for _, ev := range seedEvents(ctx, msg, "router") {
		if ev.Author != "assistant" {
			t.Errorf("author %q, want the agent that answered", ev.Author)
		}
		got = append(got, ev.Content)
	}
	want := []*genai.Content{answer.Content, result.Content, final.Content}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("seeded content (-want +got):\n%s", diff)
	}
}
