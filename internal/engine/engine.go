// Package engine builds adk agents from the roster and runs them. It is the
// only package that touches adk's runner, model and session APIs; the roster
// itself is config/default/agents/*.md.
package engine

import (
	"context"
	"fmt"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"agentlite/internal/agent"
	"agentlite/internal/config"
	"agentlite/internal/mcp"
	"agentlite/internal/proxy"
	"agentlite/internal/stream"
	"agentlite/internal/tools"
	"agentlite/internal/tools/builtin"
	"agentlite/pkg/db/opensearch"
)

// Engine implements agent.Engine on adk.
type Engine struct {
	appName      string
	defs         map[string]config.AgentDef
	registry     *proxy.Registry
	sessions     session.Service
	memory       Memory // nil when memory is not configured
	preload      bool   // search memory before every model call, not only on request
	defaultModel string // the model a run uses when the request names none
	compact      *config.Compaction
	servers      map[string]*mcp.Server // MCP servers by name
	harness      map[string]tool.Tool   // always available: task, todowrite, …
	world        map[string]tool.Tool   // switched on through connectors
	connectors   []config.Connector
	sets         map[string]tool.Toolset // one per connector
	log          zerolog.Logger
}

// New prepares an engine; Bind must follow once the run service exists.
func New(cfg *config.Config, registry *proxy.Registry, sessions session.Service, mem Memory, servers map[string]*mcp.Server, log zerolog.Logger) *Engine {
	e := &Engine{
		appName: cfg.AppName, defs: cfg.Defs(), registry: registry, sessions: sessions, memory: mem, defaultModel: cfg.Models.DefaultModel,
		compact: cfg.Models.Compaction, servers: servers, connectors: cfg.Connectors, log: log,
	}
	if m := cfg.Models.Memory; m != nil {
		e.preload = m.Preload
	}
	return e
}

// Memory is long-term memory as the engine uses it: a view per thread that
// searches past conversations and stores finished ones. pkg/memory is the
// OpenSearch implementation; any adk memory.Service can stand in.
type Memory interface {
	ForThread(threadID string) adkmemory.Service
}

// Bind builds the function tools (the task tool dispatches through d) and
// checks that every connector names tools or servers that exist.
func (e *Engine) Bind(d builtin.Dispatcher) error {
	var err error
	if e.harness, err = builtin.All(builtin.Deps{Dispatcher: d, Defs: e.defs}); err != nil {
		return err
	}
	if e.world, err = tools.World(); err != nil {
		return err
	}
	e.sets, err = e.connectorSets()
	return err
}

// Seed rebuilds a thread's session from its persisted messages.
func (e *Engine) Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error {
	return seed(ctx, e.sessions, e.appName, agent.SessionUser, threadID, agentID, history)
}

// Run executes one segment: a new user message or the answers to interrupts.
// The encoder starts first so every failure reaches the log as RUN_ERROR.
func (e *Engine) Run(ctx context.Context, in agent.Input, sink stream.Sink) (agent.Result, error) {
	run := in.Run
	enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, sink.Emit)
	enc.Start(run.TurnID, run.AgentID)
	fail := func(err error) (agent.Result, error) {
		enc.Fail(err)
		return agent.Result{Status: agent.StatusError}, err
	}
	def, ok := e.defs[run.AgentID]
	if !ok {
		return fail(fmt.Errorf("unknown agent %q", run.AgentID))
	}
	root, err := e.build(ctx, def, run)
	if err != nil {
		return fail(err)
	}
	compact, err := e.compaction(ctx, run.Model)
	if err != nil {
		return fail(err)
	}
	r, err := runner.New(runner.Config{
		AppName: e.appName, Agent: root, SessionService: e.sessions, MemoryService: e.memoryFor(run.ThreadID), Compaction: compact, AutoCreateSession: true,
	})
	if err != nil {
		return fail(err)
	}
	content, resumed, err := e.content(ctx, run.ThreadID, in)
	if err != nil {
		return fail(err)
	}
	for _, ev := range resumed {
		sink.Emit(ev)
	}

	ctx = agent.WithRun(stream.WithSink(ctx, sink), run)
	for ev, err := range r.Run(ctx, agent.SessionUser, run.ThreadID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) {
		if err != nil {
			if ctx.Err() != nil {
				enc.Cancel()
				return agent.Result{Status: agent.StatusCancelled, Text: enc.Result()}, nil
			}
			enc.Fail(err)
			return agent.Result{Status: agent.StatusError, Text: enc.Result()}, err
		}
		enc.Handle(ev)
	}
	if ctx.Err() != nil {
		enc.Cancel()
		return agent.Result{Status: agent.StatusCancelled, Text: enc.Result()}, nil
	}
	if interrupts := enc.Finish(); len(interrupts) > 0 {
		return agent.Result{Status: agent.StatusAwaitingInput, Text: enc.Result(), Interrupts: interrupts}, nil
	}
	if e.memory != nil && run.Root() {
		go e.remember(context.WithoutCancel(ctx), run.ThreadID)
	}
	return agent.Result{Status: agent.StatusDone, Text: enc.Result()}, nil
}

// memoryFor is the memory service as one thread sees it, or nil when off.
func (e *Engine) memoryFor(threadID string) adkmemory.Service {
	if e.memory == nil {
		return nil
	}
	return e.memory.ForThread(threadID)
}

// Agent builds a primary agent outside a run, for adk's own launcher: the
// default model, every connector, memory as a user-started run would have.
func (e *Engine) Agent(ctx context.Context, id string) (adkagent.Agent, error) {
	def, ok := e.defs[id]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	return e.build(ctx, def, &agent.Run{ThreadID: "dev", RootID: "dev", Model: e.defaultModel})
}

// remember stores a finished turn in long-term memory. It runs detached so
// embedding never delays the thread's release.
func (e *Engine) remember(ctx context.Context, threadID string) {
	got, err := e.sessions.Get(ctx, &session.GetRequest{AppName: e.appName, UserID: agent.SessionUser, SessionID: threadID})
	if err == nil {
		err = e.memory.ForThread(threadID).AddSessionToMemory(ctx, got.Session)
	}
	if err != nil {
		e.log.Warn().Err(err).Str("thread", threadID).Msg("memory not updated")
	}
}

// model opens the adk model for a model id, on whichever provider serves it.
func (e *Engine) model(ctx context.Context, modelID string) (model.LLM, *config.Model, error) {
	client, meta, err := e.registry.Model(modelID)
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

// content is the user message or, on resume, the function responses adk
// expects for each answered interrupt. On resume it also returns the events
// that record the user's answers in the log: an approval decision for a
// confirmation, the tool result for a client-answered tool.
func (e *Engine) content(ctx context.Context, threadID string, in agent.Input) (*genai.Content, []stream.Event, error) {
	if len(in.Resume) == 0 {
		return genai.NewContentFromText(in.Message, genai.RoleUser), nil, nil
	}
	got, err := e.sessions.Get(ctx, &session.GetRequest{AppName: e.appName, UserID: agent.SessionUser, SessionID: threadID})
	if err != nil {
		return nil, nil, err
	}
	c := &genai.Content{Role: genai.RoleUser}
	var recorded []stream.Event
	for _, entry := range in.Resume {
		call, ok := pendingCall(got.Session, entry.InterruptID)
		if !ok {
			return nil, nil, fmt.Errorf("no pending call for interrupt %q", entry.InterruptID)
		}
		original, err := toolconfirmation.OriginalCallFrom(call)
		if err != nil {
			return nil, nil, fmt.Errorf("interrupt %q: %w", entry.InterruptID, err)
		}
		payload, _ := entry.Payload.(map[string]any)
		approved, _ := payload["approved"].(bool)
		justification, _ := payload["justification"].(string)
		// task and question handle the answer themselves (a task relays it to
		// its sub-agent, approve or deny), so adk must call them again.
		confirmed := approved || original.Name == builtin.Task || original.Name == builtin.Question
		c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID: entry.InterruptID, Name: call.Name, Response: map[string]any{"confirmed": confirmed, "payload": payload},
		}})
		recorded = append(recorded, events.NewCustomEvent(stream.CustomApproval, events.WithValue(approval{ToolCallID: original.ID, Approved: confirmed, Justification: justification})))
	}
	return c, recorded, nil
}

// approval is the recorded decision for a confirmation, keyed by the real call.
type approval struct {
	ToolCallID    string `json:"toolCallId"`
	Approved      bool   `json:"approved"`
	Justification string `json:"justification,omitempty"`
}

// pendingCall finds the function call an interrupt id refers to.
func pendingCall(sess session.Session, callID string) (*genai.FunctionCall, bool) {
	evs := sess.Events()
	for i := evs.Len() - 1; i >= 0; i-- {
		ev := evs.At(i)
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p.FunctionCall != nil && p.FunctionCall.ID == callID {
				return p.FunctionCall, true
			}
		}
	}
	return nil, false
}
