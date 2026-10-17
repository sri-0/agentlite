package builtin

import (
	"fmt"

	"google.golang.org/adk/v2/tool"

	"agentlite/internal/stream"
	"agentlite/pkg/toolx"
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

// question asks the user structured questions. It pauses the run through
// adk's confirmation, which stops the agent loop even when other calls share
// the turn; the request carries the questions, and adk calls the tool again
// with the user's answers as the confirmation's output.
func question(Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: stream.ToolQuestion,
		Description: "Ask the user one to four multiple-choice questions when you need a decision only they can make. " +
			"The answers arrive as the tool result: one list per question, empty for one the user skipped. " +
			"skipped: true means the user skipped them all: go on with your best judgement and say what you assumed.",
	}, func(ctx toolx.Context, a questionArgs) (map[string]any, error) {
		if conf := ctx.ToolConfirmation(); conf != nil {
			payload, _ := conf.Payload.(map[string]any)
			answers, ok := payload["output"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("the user did not answer")
			}
			return answers, nil
		}
		if err := ctx.RequestConfirmation("Answer the questions.", map[string]any{stream.MetaToolName: stream.ToolQuestion, stream.MetaArgs: a}); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("question: %w", tool.ErrConfirmationRequired)
	})
}
