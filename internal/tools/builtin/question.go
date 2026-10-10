package builtin

import (
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type questionOption struct {
	Label       string `json:"label" jsonschema:"short answer text"`
	Description string `json:"description,omitempty" jsonschema:"what choosing this means"`
}

type questionItem struct {
	Question string           `json:"question" jsonschema:"the full question"`
	Header   string           `json:"header" jsonschema:"a few-word label for the question"`
	Options  []questionOption `json:"options" jsonschema:"2 to 4 options"`
	Multiple bool             `json:"multiple,omitempty" jsonschema:"allow several answers"`
}

type questionArgs struct {
	Questions []questionItem `json:"questions" jsonschema:"1 to 4 questions to ask the user"`
}

// question asks the user structured questions. It is long-running: the
// handler returns nothing, which adk records as a pending call, and the run
// resumes when the client supplies the answers as the tool's output.
func question(Deps) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:          "question",
		Description:   "Ask the user one to four multiple-choice questions when you need a decision only they can make. The answers arrive as the tool result.",
		IsLongRunning: true,
	}, func(adkagent.Context, questionArgs) (map[string]any, error) {
		return nil, nil
	})
}
