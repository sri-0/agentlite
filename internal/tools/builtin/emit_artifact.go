package builtin

import (
	"github.com/google/uuid"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"agentlite/internal/stream"
)

type emitArtifactArgs struct {
	ID       string `json:"id,omitempty" jsonschema:"reuse an id to update an earlier artifact"`
	Title    string `json:"title" jsonschema:"short title"`
	Kind     string `json:"kind" jsonschema:"markdown, code, html, json or csv"`
	Content  string `json:"content" jsonschema:"the artifact body"`
	Language string `json:"language,omitempty" jsonschema:"language for code artifacts"`
}

// emitArtifact publishes a standalone artifact; the encoder turns the result
// into an artifact activity keyed by id.
func emitArtifact(Deps) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        stream.ToolEmitArtifact,
		Description: "Publish a document, code file, HTML page, JSON or CSV as a standalone artifact shown beside the chat.",
	}, func(_ adkagent.Context, a emitArtifactArgs) (map[string]any, error) {
		if a.ID == "" {
			a.ID = uuid.NewString()
		}
		return map[string]any{"id": a.ID, "title": a.Title, "kind": a.Kind, "content": a.Content, "language": a.Language}, nil
	})
}
