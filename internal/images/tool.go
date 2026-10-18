package images

import (
	"agentlite/internal/artifacts"
	"agentlite/internal/stream"
	"agentlite/pkg/toolx"
)

// ToolName is the image tool, and the UI's renderer for its calls.
const ToolName = "generate_image"

type toolArgs struct {
	Prompt string `json:"prompt" jsonschema:"what the image shows, or for an edit what to change"`
	Edit   string `json:"edit,omitempty" jsonschema:"id of an image artifact of this conversation to edit; omit for a new image"`
}

// Tool is generate_image: it makes an image with the image model the user
// picked for the turn (the default otherwise), saves it as an artifact of the
// conversation and shows it in the thread.
func (s *Studio) Tool() (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: ToolName,
		Description: "Generate an image from a description, or edit an image of this conversation (its artifact id " +
			"in edit). The image is shown to the user and kept as an artifact; describe it, don't repeat it.",
		UI: ToolName,
	}, func(tc toolx.Context, a toolArgs) (map[string]any, error) {
		img, err := s.Make(tc, Request{
			RootID: tc.Thread.RootID, ThreadID: tc.Thread.ID, RunID: tc.Thread.RunID, Prompt: a.Prompt, Edit: a.Edit,
			Model: toolx.RunFrom(tc).ImageModel,
		})
		if err != nil {
			return nil, err
		}
		summary := artifacts.Summary(img)
		if sink := stream.SinkFrom(tc); sink != nil {
			sink.Emit(stream.Activity(img.ID, stream.ActivityArtifact, summary))
		}
		return summary, nil
	})
}
