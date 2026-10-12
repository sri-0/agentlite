package toolx

import (
	"context"
	"net/http"

	"google.golang.org/adk/v2/agent"
)

// Context is what a tool's function receives: adk's tool context (the call
// id, confirmations) plus the thread the call belongs to. Pass it as the
// context of the tool's HTTP requests: a client built with Headers as its
// context headers (thread_headers in agentlite's config) sends the thread.
type Context struct {
	agent.Context
	Thread Thread

	tool    string
	sources Sources
}

// Thread identifies the run a tool call belongs to.
type Thread struct {
	ID     string // the thread the run is on
	RootID string // the conversation's root thread; ID itself for a user's thread
	RunID  string
}

// Call identifies one tool call.
type Call struct {
	Thread Thread
	Tool   string // the tool's name
	ID     string // the call id the model gave it
}

// Thread headers, sent by clients that use Headers (see Context).
const (
	HeaderThread     = "X-Thread-ID"
	HeaderRootThread = "X-Root-Thread-ID"
	HeaderRun        = "X-Run-ID"
)

type runKey struct{}

type run struct {
	thread  Thread
	sources Sources
}

// WithRun returns ctx carrying the run's thread and the recorder its tools'
// sources go to. The runtime calls it once per run; sources may be nil.
func WithRun(ctx context.Context, t Thread, sources Sources) context.Context {
	return context.WithValue(ctx, runKey{}, run{thread: t, sources: sources})
}

// ThreadFrom returns the thread of the run ctx belongs to; zero outside a run.
func ThreadFrom(ctx context.Context) Thread {
	r, _ := ctx.Value(runKey{}).(run)
	return r.thread
}

// Headers returns the thread headers for ctx, or nil outside a run. It fits
// transport.Config.ContextHeaders.
func Headers(ctx context.Context) http.Header {
	t := ThreadFrom(ctx)
	if t.ID == "" {
		return nil
	}
	return http.Header{HeaderThread: {t.ID}, HeaderRootThread: {t.RootID}, HeaderRun: {t.RunID}}
}

func newContext(ctx agent.Context, tool string) Context {
	r, _ := ctx.Value(runKey{}).(run)
	return Context{Context: ctx, Thread: r.thread, tool: tool, sources: r.sources}
}

// AddSource records an item the tool returned as a source and returns its
// number. Put the number on the item the model sees; the model cites it as
// [source:N]. Outside a run nothing is recorded and the number is 0.
func (c Context) AddSource(s Source) (int, error) {
	if c.sources == nil {
		return 0, nil
	}
	return c.sources.Add(c, Call{Thread: c.Thread, Tool: c.tool, ID: c.FunctionCallID()}, s)
}
