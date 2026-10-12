package agents

import (
	"maps"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"agentlite/internal/artifacts"
	"agentlite/internal/config"
	"agentlite/internal/stream"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/toolx"
)

// mcpTools is what mcp.yaml asks of individual MCP tools, by tool name. MCP
// tools are not built with toolx, so the harness applies these through adk's
// tool callbacks instead.
type mcpTools struct {
	threadArgs map[string]string             // the argument filled with the root thread id
	ui         map[string]string             // the UI renderer
	artifacts  map[string]config.URLArtifact // where the result names a page to embed
}

func newMCPTools(cfg *config.MCPConfig) mcpTools {
	t := mcpTools{threadArgs: map[string]string{}, ui: map[string]string{}, artifacts: map[string]config.URLArtifact{}}
	for _, s := range cfg.Servers {
		maps.Copy(t.threadArgs, s.ThreadArgs)
		maps.Copy(t.ui, s.UI)
		maps.Copy(t.artifacts, s.Artifacts)
	}
	return t
}

// fillThreadArgs writes the conversation's root thread id into the argument
// mcp.yaml names, so the model can neither guess nor change it.
func (rt *Runtime) fillThreadArgs(ctx adkagent.Context, t tool.Tool, args map[string]any) (map[string]any, error) {
	if arg, ok := rt.mcpTools.threadArgs[t.Name()]; ok {
		args[arg] = toolx.ThreadFrom(ctx).RootID
	}
	return nil, nil
}

// recordURLArtifact stores a tool's result as a url artefact when mcp.yaml
// says the result names a page, and shows it in the thread. A failure is
// logged; the tool's result stands either way.
func (rt *Runtime) recordURLArtifact(ctx adkagent.Context, t tool.Tool, _, result map[string]any, err error) (map[string]any, error) {
	fields, ok := rt.mcpTools.artifacts[t.Name()]
	if !ok || err != nil {
		return nil, nil
	}
	out, _ := result["output"].(map[string]any) // adk's shape for an MCP tool's structured result
	thread := toolx.ThreadFrom(ctx)
	a := &opensearch.Artifact{
		ID: text(out[fields.ID]), Title: text(out[fields.Title]), Kind: artifacts.KindURL, URL: text(out[fields.URL]),
		RootID: thread.RootID, ThreadID: thread.ID, RunID: thread.RunID,
	}
	if a.ID == "" || a.URL == "" || a.RootID == "" {
		rt.log.Warn().Str("tool", t.Name()).Msg("no artifact recorded: the result has no id or url, or the call has no thread")
		return nil, nil
	}
	if err := rt.artifacts.Save(ctx, a, ""); err != nil {
		rt.log.Error().Err(err).Str("tool", t.Name()).Msg("record url artifact")
		return nil, nil
	}
	if sink := stream.SinkFrom(ctx); sink != nil {
		sink.Emit(stream.Activity(a.ID, stream.ActivityArtifact, artifacts.Summary(a)))
	}
	return nil, nil
}

func text(v any) string {
	s, _ := v.(string)
	return s
}
