package builtin

import (
	"fmt"

	"github.com/google/uuid"

	"agentlite/internal/artifacts"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/toolx"
)

type emitArtifactArgs struct {
	ID       string `json:"id,omitempty" jsonschema:"reuse an id to publish a new version of an earlier artifact"`
	Title    string `json:"title" jsonschema:"short title"`
	Kind     string `json:"kind" jsonschema:"markdown, code, html, json or csv"`
	Content  string `json:"content" jsonschema:"the artifact body; cite sources in it as [source:N]"`
	Language string `json:"language,omitempty" jsonschema:"language for code artifacts"`
}

// emitArtifact publishes a standalone artifact: the record and body are
// stored (internal/artifacts), and the result, a summary without the body,
// becomes an artifact activity the UI opens.
func emitArtifact(d Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name:        stream.ToolEmitArtifact,
		Description: "Publish a document, code file, HTML page, JSON or CSV as a standalone artifact shown beside the chat.",
	}, func(tc toolx.Context, a emitArtifactArgs) (map[string]any, error) {
		if a.ID == "" {
			a.ID = uuid.NewString()
		}
		if tc.Thread.RootID == "" {
			return nil, fmt.Errorf("artifacts can only be published from a thread")
		}
		art := &opensearch.Artifact{
			ID: a.ID, Title: a.Title, Kind: a.Kind, Language: a.Language,
			RootID: tc.Thread.RootID, ThreadID: tc.Thread.ID, RunID: tc.Thread.RunID,
		}
		if err := d.Artifacts.Save(tc, art, a.Content); err != nil {
			return nil, err
		}
		return artifacts.Summary(art), nil
	})
}
