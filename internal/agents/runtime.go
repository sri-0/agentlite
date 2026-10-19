// Package agents is the agent runtime: it builds adk agents from the roster and runs them. It is the
// only package that touches adk's runner, model and session APIs; the roster
// itself is config/default/agents/*.md.
package agents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool"

	"agentlite/config"
	"agentlite/internal/artifacts"
	"agentlite/internal/images"
	"agentlite/internal/mcp"
	"agentlite/internal/providers"
	"agentlite/internal/rag/userfiles"
	"agentlite/internal/refs"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/internal/sources"
	"agentlite/internal/stream"
	"agentlite/internal/tools"
	"agentlite/internal/tools/builtin"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// Runtime implements runs.Runtime on adk.
type Runtime struct {
	appName      string
	defs         map[string]config.AgentDef
	registry     *providers.Registry
	sessions     session.Service
	memory       Memory // nil when memory is not configured
	preload      bool   // search memory before every model call, not only on request
	defaultModel string // the model a run uses when the request names none
	compact      *config.Compaction
	servers      map[string]*mcp.Server // MCP servers by connector id
	sources      *sources.Recorder
	artifacts    *artifacts.Store
	skills       *skills.Library
	images       *images.Studio // nil when no image model is configured
	files        *userfiles.Store
	refs         refs.Resolvers // what a message's /name, @agent, #id refer to
	log          zerolog.Logger

	connectors []config.Connector
	transport  transport.Env           // how connectors' clients are built
	harness    map[string]toolx.Tool   // always available: subagent, todowrite, …
	skillset   tool.Toolset            // the skills library's: list_skills, load_skill, …
	world      map[string]toolx.Tool   // switched on through connectors
	sets       map[string]tool.Toolset // one per connector
	mcpTools   mcpTools                // what the mcp connectors ask of individual MCP tools
	toolUI     map[string]string       // UI renderer by tool name

	remembering sync.WaitGroup // turns being stored in memory
}

// Deps are the services the runtime uses.
type Deps struct {
	Registry  *providers.Registry
	Sessions  session.Service
	Memory    Memory                 // nil when memory is not configured
	Servers   map[string]*mcp.Server // MCP servers by connector id
	Sources   *sources.Recorder
	Skills    *skills.Library
	Artifacts *artifacts.Store
	Images    *images.Studio   // nil when no image model is configured
	Files     *userfiles.Store // uploaded files: #id, and search_userfiles when searchable
	Log       zerolog.Logger
}

// New prepares the runtime; Bind must follow once the run manager exists.
func New(cfg *config.Config, d Deps) *Runtime {
	rt := &Runtime{
		appName: cfg.AppName, defs: cfg.Defs(), registry: d.Registry, sessions: d.Sessions, memory: d.Memory,
		defaultModel: cfg.Models.DefaultModel, compact: cfg.Models.Compaction, servers: d.Servers,
		sources: d.Sources, artifacts: d.Artifacts, skills: d.Skills, images: d.Images, files: d.Files,
		refs: refs.Resolvers{'/': d.Skills, '#': d.Files}, log: d.Log,
		connectors: cfg.Connectors, transport: cfg.Transport,
		mcpTools: newMCPTools(cfg.Connectors),
	}
	if m := cfg.Models.Memory; m != nil {
		rt.preload = m.Preload
	}
	return rt
}

// Memory is long-term memory as the runtime uses it: a view per thread that
// searches past conversations and stores finished ones, and forgetting a
// thread. pkg/memory is the OpenSearch implementation.
type Memory interface {
	ForThread(threadID string) adkmemory.Service
	Forget(ctx context.Context, threadID string) error
}

// Bind builds the function tools (the subagent tool dispatches through d),
// the skills toolset, one toolset per connector, and the UI renderer of every
// tool that names one.
func (rt *Runtime) Bind(ctx context.Context, d builtin.Dispatcher) error {
	var err error
	rt.harness, err = builtin.All(builtin.Deps{
		Dispatcher: d, Defs: rt.defs, Artifacts: rt.artifacts, Skills: rt.skills, Log: rt.log,
	})
	if err != nil {
		return err
	}
	if rt.files.Searchable() { // a world tool, offered with the harness tools
		search, err := tools.SearchFiles(rt.files)
		if err != nil {
			return err
		}
		rt.harness[search.Name()] = search
	}
	if rt.images != nil { // a world tool, offered with the harness tools
		generate, err := tools.GenerateImage(rt.images)
		if err != nil {
			return err
		}
		rt.harness[generate.Name()] = generate
	}
	if rt.skillset, err = rt.skills.Toolset(ctx); err != nil {
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
	return rt.checkTools()
}

// checkTools refuses a tool an agent lists that nothing serves: not a
// harness or world tool, not the skills toolset, and not a tool an MCP
// connector lists. An MCP connector that lists none may serve any name.
func (rt *Runtime) checkTools() error {
	mcpMayServe := func(name string) bool {
		return slices.ContainsFunc(rt.connectors, func(c config.Connector) bool {
			return c.Type == config.ConnectorMCP && (len(c.MCP.Tools) == 0 || slices.Contains(c.MCP.Tools, name))
		})
	}
	for _, def := range rt.defs {
		if def.Tools == nil {
			continue
		}
		for _, name := range *def.Tools {
			if rt.tool(name) == nil && name != skills.ToolsetName && !mcpMayServe(name) {
				return fmt.Errorf("agent %q: unknown tool %q", def.ID, name)
			}
		}
	}
	return nil
}

// modelChange is the model activity's content: the turn runs on To, and the
// thread's previous turn ran on From.
type modelChange struct {
	From string `json:"from"`
	To   string `json:"to"`
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
	if run.PreviousModel != "" && in.Message != "" { // marked once, by the turn's first segment
		sink.Emit(stream.Activity("model:"+run.TurnID, stream.ActivityModel, modelChange{From: run.PreviousModel, To: run.Model}))
	}
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
	target, err := rt.registry.ResolveAs(run.Model, modelcatalog.TypeLLM)
	if err != nil {
		return fail(err)
	}
	watch := &compactionWatch{sink: sink}
	compact, err := rt.compaction(ctx, run.Model, watch)
	if err != nil {
		return fail(err)
	}
	r, err := runner.New(runner.Config{
		AppName: rt.appName, Agent: root, AutoCreateSession: true,
		SessionService: watchedSessions{Service: rt.sessions, watch: watch},
		MemoryService:  rt.memoryFor(run.ThreadID),
		Compaction:     compact,
	})
	if err != nil {
		return fail(err)
	}
	ctx = platform.WithUUIDProvider(stream.WithSink(ctx, sink), func() string { return uuid.Must(uuid.NewV7()).String() }) // adk's event ids too
	ctx = toolx.WithRun(ctx, toolx.Run{
		Thread: toolx.Thread{ID: run.ThreadID, RootID: run.RootID, RunID: run.ID},
		Model:  run.Model, Card: &target.Model, Connectors: run.Connectors, ImageModel: run.ImageModel,
		Authorization: in.Authorization,
		Sources:       rt.sources,
	})
	content, events, err := rt.content(ctx, run.ThreadID, in)
	if err != nil {
		return fail(err)
	}
	for _, ev := range events {
		sink.Emit(ev)
	}

	sse := adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}
	for ev, err := range r.Run(ctx, runs.SessionUser, run.ThreadID, content, sse) {
		if errors.Is(err, compaction.ErrCompaction) {
			// The answer is complete; only the compaction after it failed.
			watch.fail(err)
			rt.log.Warn().Err(err).Str("run", run.ID).Msg("compaction failed")
			continue
		}
		watch.next()
		if err != nil {
			watch.close()
			if ctx.Err() != nil {
				enc.Cancel()
				return runs.Result{Status: runs.StatusCancelled, Text: enc.Result()}, nil
			}
			enc.Fail(err)
			return runs.Result{Status: runs.StatusError, Text: enc.Result()}, err
		}
		enc.Handle(ev)
	}
	watch.close()
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
		rt.remembering.Go(func() { rt.remember(context.WithoutCancel(ctx), run.ThreadID) })
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

// rememberTimeout bounds storing one turn in memory.
const rememberTimeout = 2 * time.Minute

// remember stores a finished turn in long-term memory. It runs detached so
// embedding never delays the thread's release. Deleting a thread deletes its
// session first, so a turn stored after its thread was deleted is forgotten
// again rather than recalled in other conversations.
func (rt *Runtime) remember(ctx context.Context, threadID string) {
	ctx, cancel := context.WithTimeout(ctx, rememberTimeout)
	defer cancel()
	req := &session.GetRequest{AppName: rt.appName, UserID: runs.SessionUser, SessionID: threadID}
	got, err := rt.sessions.Get(ctx, req)
	if err == nil {
		err = rt.memory.ForThread(threadID).AddSessionToMemory(ctx, got.Session)
	}
	if err == nil {
		req.NumRecentEvents = 1
		if _, err = rt.sessions.Get(ctx, req); errors.Is(err, session.ErrNotFound) {
			err = rt.memory.Forget(ctx, threadID)
		}
	}
	if err != nil {
		rt.log.Warn().Err(err).Str("thread", threadID).Msg("memory not updated")
	}
}

// Wait waits for the turns still being stored in memory.
func (rt *Runtime) Wait() { rt.remembering.Wait() }

// model opens the adk model for a model id on the provider the id names,
// sending the provider its own id for the model.
func (rt *Runtime) model(ctx context.Context, modelID string) (model.LLM, *modelcatalog.Model, error) {
	t, err := rt.registry.ResolveAs(modelID, modelcatalog.TypeLLM)
	if err != nil {
		return nil, nil, err
	}
	llm, err := rt.llm(ctx, t)
	return llm, &t.Model, err
}

// llm is the adk model for a resolved target.
func (rt *Runtime) llm(ctx context.Context, t providers.Target) (model.LLM, error) {
	api := openaimodel.APIChatCompletions
	if t.API == "responses" {
		api = openaimodel.APIResponses
	}
	return openaimodel.NewModel(ctx, t.Model.UpstreamID, &openaimodel.ClientConfig{
		APIKey: t.Client.APIKey, BaseURL: t.BaseURL.String(), HTTPClient: t.Client.HTTP, API: api,
	})
}

// CheckModel reports whether a model id resolves to a model agents can run
// on; runs.Start refuses a run on one that does not.
func (rt *Runtime) CheckModel(modelID string) error {
	_, err := rt.registry.ResolveAs(modelID, modelcatalog.TypeLLM)
	return err
}
