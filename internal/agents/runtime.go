// Package agents is the agent runtime: it builds adk agents from the roster and runs them. It is the
// only package that touches adk's runner, model and session APIs; the roster
// itself is config/default/agents/*.md.
package agents

import (
	"context"
	"fmt"
	"maps"

	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"agentlite/internal/artifacts"
	"agentlite/internal/config"
	"agentlite/internal/mcp"
	"agentlite/internal/proxy"
	"agentlite/internal/runs"
	"agentlite/internal/sources"
	"agentlite/internal/stream"
	"agentlite/internal/tools/builtin"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/toolx"
)

// Runtime implements runs.Runtime on adk.
type Runtime struct {
	appName      string
	defs         map[string]config.AgentDef
	registry     *proxy.Registry
	sessions     session.Service
	memory       Memory // nil when memory is not configured
	preload      bool   // search memory before every model call, not only on request
	defaultModel string // the model a run uses when the request names none
	compact      *config.Compaction
	servers      map[string]*mcp.Server // MCP servers by name
	sources      *sources.Recorder
	artifacts    *artifacts.Store
	log          zerolog.Logger

	connectors []config.Connector
	harness    map[string]toolx.Tool   // always available: subagent, todowrite, …
	world      map[string]toolx.Tool   // switched on through connectors
	sets       map[string]tool.Toolset // one per connector
	mcpTools   mcpTools                // what mcp.yaml asks of individual MCP tools
	toolUI     map[string]string       // UI renderer by tool name
}

// Deps are the services the runtime uses.
type Deps struct {
	Registry  *proxy.Registry
	Sessions  session.Service
	Memory    Memory                 // nil when memory is not configured
	Servers   map[string]*mcp.Server // MCP servers by name
	Sources   *sources.Recorder
	Artifacts *artifacts.Store
	Log       zerolog.Logger
}

// New prepares the runtime; Bind must follow once the run manager exists.
func New(cfg *config.Config, d Deps) *Runtime {
	rt := &Runtime{
		appName: cfg.AppName, defs: cfg.Defs(), registry: d.Registry, sessions: d.Sessions, memory: d.Memory,
		defaultModel: cfg.Models.DefaultModel, compact: cfg.Models.Compaction, servers: d.Servers,
		sources: d.Sources, artifacts: d.Artifacts, log: d.Log, connectors: cfg.Connectors,
		mcpTools: newMCPTools(cfg.MCP),
	}
	if m := cfg.Models.Memory; m != nil {
		rt.preload = m.Preload
	}
	return rt
}

// Memory is long-term memory as the runtime uses it: a view per thread that
// searches past conversations and stores finished ones. pkg/memory is the
// OpenSearch implementation; any adk memory.Service can stand in.
type Memory interface {
	ForThread(threadID string) adkmemory.Service
}

// Bind builds the function tools (the subagent tool dispatches through d),
// one toolset per connector, and the UI renderer of every tool that names one.
func (rt *Runtime) Bind(d builtin.Dispatcher) error {
	var err error
	rt.harness, err = builtin.All(builtin.Deps{Dispatcher: d, Defs: rt.defs, Artifacts: rt.artifacts, Log: rt.log})
	if err != nil {
		return err
	}
	if err := rt.connectorSets(); err != nil {
		return err
	}
	rt.toolUI = maps.Clone(rt.mcpTools.ui)
	for _, set := range []map[string]toolx.Tool{rt.harness, rt.world} {
		for name, t := range set {
			if t.UI() != "" {
				rt.toolUI[name] = t.UI()
			}
		}
	}
	return nil
}

// Seed rebuilds a thread's session from its persisted messages.
func (rt *Runtime) Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error {
	return seed(ctx, rt.sessions, rt.appName, runs.SessionUser, threadID, agentID, history)
}

// Run executes one segment: a new user message or the answers to interrupts.
// The encoder starts first so every failure reaches the log as RUN_ERROR.
func (rt *Runtime) Run(ctx context.Context, in runs.Input, sink stream.Sink) (runs.Result, error) {
	run := in.Run
	enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, sink.Emit)
	enc.ToolUI = rt.toolUI
	enc.Start(run.TurnID, run.AgentID)
	fail := func(err error) (runs.Result, error) {
		enc.Fail(err)
		return runs.Result{Status: runs.StatusError}, err
	}
	def, ok := rt.defs[run.AgentID]
	if !ok {
		return fail(fmt.Errorf("unknown agent %q", run.AgentID))
	}
	root, err := rt.build(ctx, def, run)
	if err != nil {
		return fail(err)
	}
	compact, err := rt.compaction(ctx, run.Model)
	if err != nil {
		return fail(err)
	}
	r, err := runner.New(runner.Config{
		AppName: rt.appName, Agent: root, SessionService: rt.sessions, MemoryService: rt.memoryFor(run.ThreadID), Compaction: compact, AutoCreateSession: true,
	})
	if err != nil {
		return fail(err)
	}
	content, resumed, err := rt.content(ctx, run.ThreadID, in)
	if err != nil {
		return fail(err)
	}
	for _, ev := range resumed {
		sink.Emit(ev)
	}

	ctx = runs.WithRun(stream.WithSink(ctx, sink), run)
	ctx = toolx.WithRun(ctx, toolx.Thread{ID: run.ThreadID, RootID: run.RootID, RunID: run.ID}, rt.sources)
	for ev, err := range r.Run(ctx, runs.SessionUser, run.ThreadID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) {
		if err != nil {
			if ctx.Err() != nil {
				enc.Cancel()
				return runs.Result{Status: runs.StatusCancelled, Text: enc.Result()}, nil
			}
			enc.Fail(err)
			return runs.Result{Status: runs.StatusError, Text: enc.Result()}, err
		}
		enc.Handle(ev)
	}
	if ctx.Err() != nil {
		enc.Cancel()
		return runs.Result{Status: runs.StatusCancelled, Text: enc.Result()}, nil
	}
	// Citations are recorded before the segment's closing event, so a client
	// that reads the sources when the run ends sees them.
	if err := rt.sources.Cite(ctx, run.RootID, "message:"+run.TurnID, enc.Shown()); err != nil {
		rt.log.Warn().Err(err).Str("run", run.ID).Msg("record citations")
	}
	if interrupts := enc.Finish(); len(interrupts) > 0 {
		return runs.Result{Status: runs.StatusAwaitingInput, Text: enc.Result(), Interrupts: interrupts}, nil
	}
	if rt.memory != nil && run.Root() {
		go rt.remember(context.WithoutCancel(ctx), run.ThreadID)
	}
	return runs.Result{Status: runs.StatusDone, Text: enc.Result()}, nil
}

// memoryFor is the memory service as one thread sees it, or nil when off.
func (rt *Runtime) memoryFor(threadID string) adkmemory.Service {
	if rt.memory == nil {
		return nil
	}
	return rt.memory.ForThread(threadID)
}

// Agent builds a primary agent outside a run, for adk's own launcher: the
// default model, every connector, memory as a user-started run would have.
func (rt *Runtime) Agent(ctx context.Context, id string) (adkagent.Agent, error) {
	def, ok := rt.defs[id]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	return rt.build(ctx, def, &runs.Run{ThreadID: "dev", RootID: "dev", Model: rt.defaultModel})
}

// remember stores a finished turn in long-term memory. It runs detached so
// embedding never delays the thread's release.
func (rt *Runtime) remember(ctx context.Context, threadID string) {
	got, err := rt.sessions.Get(ctx, &session.GetRequest{AppName: rt.appName, UserID: runs.SessionUser, SessionID: threadID})
	if err == nil {
		err = rt.memory.ForThread(threadID).AddSessionToMemory(ctx, got.Session)
	}
	if err != nil {
		rt.log.Warn().Err(err).Str("thread", threadID).Msg("memory not updated")
	}
}

// model opens the adk model for a model id, on whichever provider serves it.
func (rt *Runtime) model(ctx context.Context, modelID string) (model.LLM, *config.Model, error) {
	client, meta, err := rt.registry.Model(modelID)
	if err != nil {
		return nil, nil, err
	}
	api := openaimodel.APIChatCompletions
	if client.Config.API == "responses" {
		api = openaimodel.APIResponses
	}
	llm, err := openaimodel.NewModel(ctx, modelID, &openaimodel.ClientConfig{
		APIKey: client.APIKey, BaseURL: client.BaseURL.String(), HTTPClient: client.HTTP, API: api,
	})
	return llm, meta, err
}
