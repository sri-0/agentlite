package toolx

import (
	"context"
	"net/http"

	"google.golang.org/adk/v2/agent"

	"agentlite/pkg/auth"
	"agentlite/pkg/citations"
	"agentlite/pkg/modelcatalog"
)

// Context is what a tool's function receives: adk's tool context (the call
// id, confirmations) plus the run the call belongs to. Pass it as the context
// of the tool's HTTP requests: a connector that forwards identity sends the
// user's Authorization header, the thread and run, and this agent and tool.
type Context struct {
	agent.Context
	Thread Thread

	tool    string
	sources citations.Recorder
}

// Run is what the harness knows about the run a tool call belongs to.
type Run struct {
	Thread     Thread
	Model      string              // the model the run uses; a sub-agent runs on it too
	Card       *modelcatalog.Model // that model's card: what it takes as input, among the rest
	Connectors *[]string           // the connectors switched on; nil means all
	ImageModel string              // the image model the user picked for generate_image; "" is the default
	// User is who the run is for, with their access token as the request
	// that queued the run's segment carried it; a sub-agent runs for them too.
	User    auth.Principal
	Sources citations.Recorder // nil outside a run: nothing is recorded
}

// Thread identifies the run a tool call belongs to.
type Thread struct {
	ID     string // the thread the run is on
	RootID string // the conversation's root thread; ID itself for a user's thread
	RunID  string
}

type (
	runKey  struct{}
	callKey struct{}
)

// caller is the agent and tool of one call, as requests made with its
// Context see them.
type caller struct{ agent, tool string }

// WithRun returns ctx carrying the run every tool called under it belongs to.
// The runtime calls it once per run.
func WithRun(ctx context.Context, r Run) context.Context {
	return context.WithValue(ctx, runKey{}, r)
}

// RunFrom returns the run ctx belongs to; zero outside a run.
func RunFrom(ctx context.Context) Run {
	r, _ := ctx.Value(runKey{}).(Run)
	return r
}

// Run headers: the thread, run, agent and tool a tool call's requests are
// made for, so a downstream system can scope, audit or authorise by thread.
const (
	HeaderThread     = "X-Thread-ID"
	HeaderRootThread = "X-Root-Thread-ID"
	HeaderRun        = "X-Run-ID"
	HeaderAgent      = "X-Agent-ID"
	HeaderTool       = "X-Tool-Name"
)

// Headers returns the headers a request made with ctx carries when its
// connector forwards identity: the user's Authorization and the run headers,
// the agent and tool only from a tool's Context. It fits
// transport.Config.ContextHeaders.
func Headers(ctx context.Context) http.Header {
	h := http.Header{}
	if r := RunFrom(ctx); r.Thread.ID != "" {
		h.Set(HeaderThread, r.Thread.ID)
		h.Set(HeaderRootThread, r.Thread.RootID)
		h.Set(HeaderRun, r.Thread.RunID)
		if b := r.User.Bearer(); b != "" {
			h.Set("Authorization", b)
		}
	}
	if c, ok := ctx.Value(callKey{}).(caller); ok {
		h.Set(HeaderAgent, c.agent)
		h.Set(HeaderTool, c.tool)
	}
	return h
}

func newContext(ctx agent.Context, tool string) Context {
	r := RunFrom(ctx)
	return Context{Context: ctx, Thread: r.Thread, tool: tool, sources: r.Sources}
}

// Value answers for the call itself, so requests made with the Context carry
// its agent and tool; everything else is adk's context.
func (c Context) Value(key any) any {
	if key == (callKey{}) {
		return caller{agent: c.AgentName(), tool: c.tool}
	}
	return c.Context.Value(key)
}

// AddSource records an item the tool returned as a source and returns its
// number. Put the number on the item the model sees; the model cites it as
// [source:N]. Outside a run nothing is recorded and the number is 0.
func (c Context) AddSource(s citations.Source) (int, error) {
	if c.sources == nil {
		return 0, nil
	}
	return c.sources.Add(c, citations.Origin{
		ThreadID: c.Thread.ID, RootID: c.Thread.RootID, RunID: c.Thread.RunID, Tool: c.tool, CallID: c.FunctionCallID(),
	}, s)
}
