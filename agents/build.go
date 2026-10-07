package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/agent/workflowagents/parallelagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/loadmemorytool"
	"google.golang.org/adk/v2/tool/preloadmemorytool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"agentlite/internal/config"
	"agentlite/internal/tools"
)

// defaultMaxOutputTokens is always sent: some gateways reject requests without it.
const defaultMaxOutputTokens = 8192

// build turns a definition into an adk agent for one run. Workflow kinds
// build their steps recursively; model is the run's model override, which
// applies to every llm agent that does not name its own.
func (e *Engine) build(ctx context.Context, def config.AgentDef, model string) (adkagent.Agent, error) {
	switch def.Kind {
	case config.KindLLM:
		return e.buildLLM(ctx, def, model)
	case config.KindSequential, config.KindParallel, config.KindLoop:
		subs, err := e.buildSteps(ctx, def, model)
		if err != nil {
			return nil, err
		}
		cfg := adkagent.Config{Name: def.ID, Description: def.Description, SubAgents: subs}
		switch def.Kind {
		case config.KindSequential:
			return sequentialagent.New(sequentialagent.Config{AgentConfig: cfg})
		case config.KindParallel:
			return parallelagent.New(parallelagent.Config{AgentConfig: cfg})
		default:
			return loopagent.New(loopagent.Config{AgentConfig: cfg, MaxIterations: uint(def.MaxIterations)})
		}
	case config.KindGraph:
		return e.buildGraph(ctx, def, model)
	default:
		return nil, fmt.Errorf("agent %q: unknown kind %q", def.ID, def.Kind)
	}
}

func (e *Engine) buildLLM(ctx context.Context, def config.AgentDef, model string) (adkagent.Agent, error) {
	if def.Model != "" {
		model = def.Model
	}
	llm, _, err := e.model(ctx, def.Provider, model)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", def.ID, err)
	}
	fns, toolsets := e.toolsFor(def)
	if e.memory != nil && def.Primary() {
		fns = append(fns, preloadmemorytool.New(), loadmemorytool.New())
	}
	maxTokens := def.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	return llmagent.New(llmagent.Config{
		Name:                     def.ID,
		Description:              def.Description,
		Model:                    llm,
		Instruction:              e.instruction(def),
		Tools:                    fns,
		Toolsets:                 toolsets,
		OutputKey:                def.OutputKey,
		GenerateContentConfig:    &genai.GenerateContentConfig{MaxOutputTokens: int32(maxTokens)},
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
}

func (e *Engine) buildSteps(ctx context.Context, def config.AgentDef, model string) ([]adkagent.Agent, error) {
	if len(def.Subagents) == 0 {
		return nil, fmt.Errorf("agent %q: %s kind needs subagents", def.ID, def.Kind)
	}
	steps := make([]adkagent.Agent, 0, len(def.Subagents))
	for _, id := range def.Subagents {
		step, err := e.build(ctx, e.defs[id], model)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// buildGraph wires the subagents as nodes with the declared edges. An edge's
// route, when set, is matched against the values a node puts in Event.Routes.
func (e *Engine) buildGraph(ctx context.Context, def config.AgentDef, model string) (adkagent.Agent, error) {
	steps, err := e.buildSteps(ctx, def, model)
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]workflow.Node, len(steps))
	for _, a := range steps {
		node, err := workflow.NewAgentNode(a, workflow.NodeConfig{})
		if err != nil {
			return nil, fmt.Errorf("agent %q: node %s: %w", def.ID, a.Name(), err)
		}
		nodes[a.Name()] = node
	}
	edges := workflow.NewEdgeBuilder()
	for _, edge := range def.Edges {
		from, to := nodes[edge.From], nodes[edge.To]
		if from == nil || to == nil {
			return nil, fmt.Errorf("agent %q: edge %s→%s names an unknown subagent", def.ID, edge.From, edge.To)
		}
		if edge.Route == "" {
			edges.Add(from, to)
		} else {
			edges.AddRoute(from, to, routeValue(edge.Route))
		}
	}
	return workflowagent.New(workflowagent.Config{Name: def.ID, Description: def.Description, SubAgents: steps, Edges: edges.Build()})
}

// routeValue selects an edge when the node's event lists that value in Routes.
type routeValue string

func (r routeValue) Matches(ev *session.Event) bool { return slices.Contains(ev.Routes, string(r)) }

// toolsFor resolves an agent's tools. Omitted means every function tool and
// MCP tool; the task tool is included only for agents that declare subagents.
func (e *Engine) toolsFor(def config.AgentDef) ([]tool.Tool, []tool.Toolset) {
	if def.Tools == nil {
		var fns []tool.Tool
		for name, t := range e.tools {
			if name == tools.Task && len(def.Subagents) == 0 {
				continue
			}
			fns = append(fns, t)
		}
		return fns, e.toolsets
	}
	var fns []tool.Tool
	var mcpNames []string
	for _, name := range *def.Tools {
		if t, ok := e.tools[name]; ok {
			fns = append(fns, t)
		} else {
			mcpNames = append(mcpNames, name)
		}
	}
	if len(mcpNames) == 0 {
		return fns, nil
	}
	sets := make([]tool.Toolset, 0, len(e.toolsets))
	for _, ts := range e.toolsets {
		sets = append(sets, tool.FilterToolset(ts, tool.AllowedToolsPredicate(mcpNames)))
	}
	return fns, sets
}

// instruction appends the sub-agent manifest the task tool relies on.
func (e *Engine) instruction(def config.AgentDef) string {
	if len(def.Subagents) == 0 || def.Tools != nil && !slices.Contains(*def.Tools, tools.Task) {
		return def.Instruction
	}
	var b strings.Builder
	b.WriteString(def.Instruction)
	b.WriteString("\n\n<available_subagents>\n")
	for _, id := range slices.Sorted(slices.Values(def.Subagents)) {
		sub := e.defs[id]
		fmt.Fprintf(&b, "- %s: %s\n", sub.ID, sub.Description)
	}
	b.WriteString("</available_subagents>")
	return b.String()
}
