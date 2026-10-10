package tools

import (
	"log/slog"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type writeNoteArgs struct {
	Title string `json:"title" jsonschema:"note title"`
	Body  string `json:"body" jsonschema:"note body in markdown"`
}

// writeNote is the approval demo: adk pauses the run until the user approves,
// and the user's justification arrives in the confirmation payload.
func writeNote() (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:                "write_note",
		Description:         "Save a note for the user. Requires the user's approval.",
		RequireConfirmation: true,
	}, func(ctx adkagent.Context, a writeNoteArgs) (map[string]any, error) {
		justification := ""
		if c := ctx.ToolConfirmation(); c != nil {
			if p, ok := c.Payload.(map[string]any); ok {
				justification, _ = p["justification"].(string)
			}
		}
		slog.Info("note written", "title", a.Title, "justification", justification)
		return map[string]any{"ok": true, "title": a.Title, "justification": justification}, nil
	})
}
