package builtin

import "agentlite/pkg/toolx"

type reportProgressArgs struct {
	Message string `json:"message" jsonschema:"one line on what you are doing now"`
}

// progressTool is the name the subagent tool watches for in a sub-agent's log.
const progressTool = "report_progress"

// reportProgress has no effect of its own: its call in a sub-agent's log is
// what the parent's subagent call lifts into the agent card.
func reportProgress(Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name:        progressTool,
		Description: "Report a one-line progress update to whoever dispatched you.",
	}, func(toolx.Context, reportProgressArgs) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
}
