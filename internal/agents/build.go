package agents

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/genai"

	"agentlite/internal/config"
	"agentlite/internal/runs"
	"agentlite/internal/tools/builtin"
)

// defaultMaxOutputTokens is always sent: some gateways reject requests without it.
const defaultMaxOutputTokens = 8192

// build turns a definition into an adk agent for one run: an llm agent, or a
// sequential pipeline of its subagents. Every llm agent uses the run's model.
func (rt *Runtime) build(ctx context.Context, def config.AgentDef, run *runs.Run) (adkagent.Agent, error) {
	if def.Kind == config.KindLLM {
		return rt.buildLLM(ctx, def, run)
	}
	steps := make([]adkagent.Agent, 0, len(def.Subagents))
	for _, id := range def.Subagents {
		step, err := rt.build(ctx, rt.defs[id], run)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return sequentialagent.New(sequentialagent.Config{AgentConfig: adkagent.Config{Name: def.ID, Description: def.Description, SubAgents: steps}})
}

func (rt *Runtime) buildLLM(ctx context.Context, def config.AgentDef, run *runs.Run) (adkagent.Agent, error) {
	llm, _, err := rt.model(ctx, run.Model)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", def.ID, err)
	}
	fns, toolsets, err := rt.toolsFor(def, run)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", def.ID, err)
	}
	if rt.memory != nil && run.Root() { // sub-agent runs work from their prompt alone
		fns = append(fns, loadmemorytool.New())
		if rt.preload {
			fns = append(fns, preloadmemorytool.New())
		}
	}
	maxTokens := def.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	return llmagent.New(llmagent.Config{
		Name:                     def.ID,
		Description:              def.Description,
		Model:                    llm,
		Instruction:              rt.instruction(def),
		Tools:                    fns,
		Toolsets:                 toolsets,
		GenerateContentConfig:    &genai.GenerateContentConfig{MaxOutputTokens: int32(maxTokens)},
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
}

// toolsFor resolves an agent's tools for a run. A listed name must be a
// harness or world tool or, when MCP servers are configured, is taken to be
// one of their tools. Omitted means every harness tool plus the toolsets of
// the run's connectors; the subagent tool only for agents with subagents.
func (rt *Runtime) toolsFor(def config.AgentDef, run *runs.Run) ([]tool.Tool, []tool.Toolset, error) {
	var fns []tool.Tool
	if def.Tools != nil {
		var mcpNames []string
		for _, name := range *def.Tools {
			switch t := rt.tool(name); {
			case t != nil:
				fns = append(fns, t)
			case len(rt.servers) > 0:
				mcpNames = append(mcpNames, name)
			default:
				return nil, nil, fmt.Errorf("unknown tool %q", name)
			}
		}
		if len(mcpNames) == 0 {
			return fns, nil, nil
		}
		var sets []tool.Toolset
		for _, srv := range rt.servers {
			sets = append(sets, tool.FilterToolset(srv.Toolset, tool.AllowedToolsPredicate(mcpNames)))
		}
		return fns, sets, nil
	}
	for _, name := range slices.Sorted(maps.Keys(rt.harness)) {
		if name == builtin.Subagent && len(def.Subagents) == 0 {
			continue
		}
		if !run.Root() && orchestration[name] { // like opencode: sub-agents do the work, they do not delegate or ask
			continue
		}
		fns = append(fns, rt.harness[name])
	}
	return fns, rt.enabled(run.Connectors), nil
}

// toolList names what a sub-agent can call, so the parent delegates only
// work it can do. A pipeline lists its steps' tools.
func (rt *Runtime) toolList(def config.AgentDef) string {
	var names []string
	if def.Kind != config.KindLLM {
		for _, id := range def.Subagents {
			names = append(names, rt.toolList(rt.defs[id]))
		}
	} else if def.Tools != nil {
		names = *def.Tools
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// orchestration are the harness tools a sub-agent only gets by listing them.
var orchestration = map[string]bool{builtin.Subagent: true, "todowrite": true, builtin.Question: true}

func (rt *Runtime) tool(name string) tool.Tool {
	if t, ok := rt.harness[name]; ok {
		return t
	}
	return rt.world[name]
}

// instruction appends the sub-agent manifest the subagent tool relies on.
func (rt *Runtime) instruction(def config.AgentDef) string {
	if len(def.Subagents) == 0 || def.Tools != nil && !slices.Contains(*def.Tools, builtin.Subagent) {
		return def.Instruction
	}
	var b strings.Builder
	b.WriteString(def.Instruction)
	b.WriteString("\n\n<available_subagents>\n")
	for _, id := range slices.Sorted(slices.Values(def.Subagents)) {
		sub := rt.defs[id]
		fmt.Fprintf(&b, "- %s: %s (tools: %s)\n", sub.ID, sub.Description, rt.toolList(sub))
	}
	b.WriteString("</available_subagents>")
	return b.String()
}
