// Package agents builds adk agents from the roster and runs them. It is the
// only package that touches adk's runner, model and session APIs.
package agents

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"agentlite/internal/agent"
	"agentlite/internal/config"
	"agentlite/internal/proxy"
	"agentlite/internal/stream"
	"agentlite/internal/tools"
	"agentlite/pkg/db/opensearch"
)

// sessionUser is adk's required user id; agentlite has no identity yet.
const sessionUser = "default"

// Engine implements agent.Engine on adk.
type Engine struct {
	appName  string
	defs     map[string]config.AgentDef
	registry *proxy.Registry
	sessions session.Service
	memory   memory.Service // nil when memory is not configured
	toolsets []tool.Toolset
	tools    map[string]tool.Tool
	log      zerolog.Logger
}

// New prepares an engine; Bind must follow once the run service exists.
func New(cfg *config.Config, registry *proxy.Registry, sessions session.Service, mem memory.Service, toolsets []tool.Toolset, log zerolog.Logger) *Engine {
	defs := make(map[string]config.AgentDef, len(cfg.Agents))
	for _, d := range cfg.Agents {
		defs[d.ID] = d
	}
	return &Engine{appName: cfg.AppName, defs: defs, registry: registry, sessions: sessions, memory: mem, toolsets: toolsets, log: log}
}

// Bind builds the function tools; the task tool dispatches through d.
func (e *Engine) Bind(d tools.Dispatcher) error {
	set, err := tools.All(tools.Deps{Dispatcher: d, Defs: e.defs})
	if err != nil {
		return err
	}
	e.tools = set
	return nil
}

// Seed rebuilds a thread's session from its persisted messages.
func (e *Engine) Seed(ctx context.Context, threadID, agentID string, history []opensearch.Message) error {
	return seed(ctx, e.sessions, e.appName, sessionUser, threadID, agentID, history)
}

// Run executes one segment: a new user message or the answers to interrupts.
// The encoder starts first so every failure reaches the log as RUN_ERROR.
func (e *Engine) Run(ctx context.Context, in agent.Input, sink stream.Sink) (agent.Status, string, error) {
	run := in.Run
	enc := stream.NewEncoder(run.ThreadID, run.ID, run.Model, sink.Emit)
	enc.Start(run.TurnID, run.AgentID)
	fail := func(err error) (agent.Status, string, error) {
		enc.Fail(err)
		return agent.StatusError, "", err
	}
	def, ok := e.defs[run.AgentID]
	if !ok {
		return fail(fmt.Errorf("unknown agent %q", run.AgentID))
	}
	root, err := e.build(ctx, def, run.Model)
	if err != nil {
		return fail(err)
	}
	compact, err := e.compaction(ctx, def, run.Model)
	if err != nil {
		return fail(err)
	}
	r, err := runner.New(runner.Config{
		AppName: e.appName, Agent: root, SessionService: e.sessions, MemoryService: e.memory, Compaction: compact, AutoCreateSession: true,
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

	ctx = stream.WithSink(ctx, sink)
	for ev, err := range r.Run(ctx, sessionUser, run.ThreadID, content, adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}) {
		if err != nil {
			if ctx.Err() != nil {
				enc.Cancel()
				return agent.StatusCancelled, enc.Result(), nil
			}
			enc.Fail(err)
			return agent.StatusError, enc.Result(), err
		}
		enc.Handle(ev)
	}
	if ctx.Err() != nil {
		enc.Cancel()
		return agent.StatusCancelled, enc.Result(), nil
	}
	if len(enc.Finish()) > 0 {
		return agent.StatusAwaitingInput, enc.Result(), nil
	}
	if e.memory != nil && def.Primary() {
		go e.remember(context.WithoutCancel(ctx), run.ThreadID)
	}
	return agent.StatusDone, enc.Result(), nil
}

// remember stores a finished turn in long-term memory. It runs detached so
// embedding never delays the thread's release.
func (e *Engine) remember(ctx context.Context, threadID string) {
	got, err := e.sessions.Get(ctx, &session.GetRequest{AppName: e.appName, UserID: sessionUser, SessionID: threadID})
	if err == nil {
		err = e.memory.AddSessionToMemory(ctx, got.Session)
	}
	if err != nil {
		e.log.Warn().Err(err).Str("thread", threadID).Msg("memory not updated")
	}
}

// compaction builds adk's compaction config for agents that ask for it; the
// token threshold is a fraction of the model's context window.
func (e *Engine) compaction(ctx context.Context, def config.AgentDef, modelID string) (*compaction.Config, error) {
	c := def.Compaction
	if c == nil {
		return nil, nil
	}
	if def.Model != "" {
		modelID = def.Model
	}
	llm, meta, err := e.model(ctx, def.Provider, modelID)
	if err != nil {
		return nil, err
	}
	summarizer, err := compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{Model: llm})
	if err != nil {
		return nil, err
	}
	cfg := &compaction.Config{CompactionInterval: c.Interval, OverlapSize: c.Overlap, Summarizer: summarizer}
	if c.TokenThreshold > 0 {
		cfg.TokenThreshold = int(c.TokenThreshold * float64(meta.ContextLength))
		cfg.EventRetentionSize = retainedEvents
	}
	return cfg, nil
}

// retainedEvents is how many recent events a token-threshold compaction keeps
// verbatim so the turn being answered is never summarised away.
const retainedEvents = 20

func (e *Engine) model(ctx context.Context, providerID, modelID string) (model.LLM, *config.Model, error) {
	client, meta, err := e.registry.Model(providerID, modelID)
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
	got, err := e.sessions.Get(ctx, &session.GetRequest{AppName: e.appName, UserID: sessionUser, SessionID: threadID})
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
		response := responseFor(call.Name, entry)
		c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: entry.InterruptID, Name: call.Name, Response: response}})
		if call.Name != toolconfirmation.FunctionCallName {
			body, _ := json.Marshal(response)
			recorded = append(recorded, events.NewToolCallResultEvent(uuid.NewString(), call.ID, string(body)))
			continue
		}
		if original, err := toolconfirmation.OriginalCallFrom(call); err == nil {
			payload, _ := entry.Payload.(map[string]any)
			approved, _ := payload["approved"].(bool)
			justification, _ := payload["justification"].(string)
			recorded = append(recorded, events.NewCustomEvent(stream.CustomApproval, events.WithValue(approval{ToolCallID: original.ID, Approved: approved, Justification: justification})))
		}
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

// responseFor shapes the resume payload the way adk expects: a confirmation
// carries {confirmed, payload}; a client-answered tool gets its output.
func responseFor(name string, entry types.ResumeEntry) map[string]any {
	payload, _ := entry.Payload.(map[string]any)
	if name == toolconfirmation.FunctionCallName {
		approved, _ := payload["approved"].(bool)
		return map[string]any{"confirmed": approved && entry.Status == types.ResumeStatusResolved, "payload": payload}
	}
	if payload == nil {
		return map[string]any{"result": entry.Payload}
	}
	return payload
}
