package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/google/uuid"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"agentlite/internal/agent"
	"agentlite/internal/stream"
)

const (
	maxChildren  = 4
	progressKeep = 10
)

// children caps how many sub-agents run at once across the process.
var children = make(chan struct{}, maxChildren)

type taskArgs struct {
	SubagentType string `json:"subagent_type" jsonschema:"id of the sub-agent to run, from available_subagents"`
	Description  string `json:"description" jsonschema:"3 to 6 word label for the task"`
	Prompt       string `json:"prompt" jsonschema:"the complete task; the sub-agent sees nothing else"`
}

// agentCard is the parent's view of a sub-agent, sent as an agent activity.
type agentCard struct {
	SubagentRunID    string         `json:"subagentRunId"`
	ChildThreadID    string         `json:"childThreadId"`
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	ParentToolCallID string         `json:"parentToolCallId"`
	Status           string         `json:"status"`
	StartedAt        int64          `json:"startedAt"`
	DurationMs       int64          `json:"durationMs,omitempty"`
	Error            string         `json:"error,omitempty"`
	Progress         string         `json:"progress,omitempty"`
	Log              []progressLine `json:"log,omitempty"`
	Text             string         `json:"text,omitempty"`
}

type progressLine struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}

// task runs a sub-agent on its own child thread and returns its answer. The
// child's activity stays on the child thread; the parent only gets a card
// with status and a bounded progress log.
func task(d Deps) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        Task,
		Description: "Delegate a self-contained task to a sub-agent and get its result. Several task calls in one turn run in parallel.",
	}, func(ctx adkagent.Context, a taskArgs) (map[string]any, error) {
		parent := d.Defs[ctx.AgentName()]
		if !slices.Contains(parent.Subagents, a.SubagentType) {
			return nil, fmt.Errorf("unknown subagent_type %q; choose one of %v", a.SubagentType, parent.Subagents)
		}
		sink, parentRun := stream.SinkFrom(ctx), agent.RunFrom(ctx)
		if sink == nil || parentRun == nil {
			return nil, fmt.Errorf("task tool needs a run context")
		}
		select {
		case children <- struct{}{}:
			defer func() { <-children }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		childThread := uuid.Must(uuid.NewV7()).String()
		run, err := d.Dispatcher.Start(ctx, agent.StartInput{
			ThreadID: childThread, ParentID: ctx.SessionID(), AgentID: a.SubagentType, Model: parentRun.Model,
			Connectors: parentRun.Connectors, MessageID: uuid.NewString(), Text: a.Prompt,
		})
		if err != nil {
			return nil, fmt.Errorf("start %s: %w", a.SubagentType, err)
		}
		card := &cardState{card: agentCard{
			SubagentRunID: run.ID, ChildThreadID: childThread, Name: a.SubagentType, Description: a.Description,
			ParentToolCallID: ctx.FunctionCallID(), Status: "running", StartedAt: time.Now().UnixMilli(),
		}, sink: sink}
		sink.Emit(events.NewSubagentStartedEvent(run.ID, a.SubagentType, events.WithSubagentDescription(a.Description), events.WithParentToolCall(ctx.FunctionCallID(), "")))
		card.emit()

		tailCtx, stopTail := context.WithCancel(ctx)
		go tailProgress(tailCtx, d.Dispatcher, childThread, card)
		final, err := d.Dispatcher.Wait(ctx, run.ID)
		stopTail()
		if err != nil { // the parent was cancelled or Valkey failed: the child must not run on unseen
			_ = d.Dispatcher.Cancel(context.WithoutCancel(ctx), childThread)
			card.finish("error", "", err.Error())
			sink.Emit(events.NewSubagentErrorEvent(run.ID, err.Error()))
			return nil, err
		}
		if final.Status != agent.StatusDone {
			msg := final.Error
			if msg == "" {
				msg = string(final.Status)
			}
			if final.Status == agent.StatusAwaitingInput { // nobody can answer a sub-agent; let its thread go
				msg = "sub-agent paused for input it cannot receive"
				if err := d.Dispatcher.Cancel(ctx, childThread); err != nil {
					msg += " (" + err.Error() + ")"
				}
			}
			card.finish("error", final.Result, msg)
			sink.Emit(events.NewSubagentErrorEvent(run.ID, msg))
			return nil, fmt.Errorf("%s %s: %s", a.SubagentType, final.Status, msg)
		}
		card.finish("done", final.Result, "")
		sink.Emit(events.NewSubagentFinishedEvent(run.ID, events.WithSubagentSuccessOutcome(), events.WithSubagentResult(final.Result)))
		return map[string]any{"result": final.Result}, nil
	})
}

// cardState serialises updates to the card from the tail goroutine and the
// tool's own goroutine.
type cardState struct {
	mu   sync.Mutex
	card agentCard
	sink stream.Sink
}

func (c *cardState) emit() {
	c.sink.Emit(stream.Activity(c.card.SubagentRunID, stream.ActivityAgent, c.card))
}

func (c *cardState) progress(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.card.Progress = message
	c.card.Log = append(c.card.Log, progressLine{At: time.Now().UnixMilli(), Message: message})
	if len(c.card.Log) > progressKeep {
		c.card.Log = c.card.Log[len(c.card.Log)-progressKeep:]
	}
	c.emit()
}

func (c *cardState) finish(status, text, errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.card.Status, c.card.Text, c.card.Error = status, text, errText
	c.card.DurationMs = time.Now().UnixMilli() - c.card.StartedAt
	c.emit()
}

// tailProgress reads the child's log and turns each tool call into one
// progress line; report_progress calls contribute their message verbatim.
func tailProgress(ctx context.Context, d Dispatcher, threadID string, card *cardState) {
	entries, err := d.Subscribe(ctx, threadID, "")
	if err != nil || entries == nil {
		return
	}
	names := map[string]string{}
	args := map[string]string{}
	for e, err := range entries {
		if err != nil {
			return
		}
		switch ev := e.Event.(type) {
		case *events.ToolCallStartEvent:
			names[ev.ToolCallID] = ev.ToolCallName
		case *events.ToolCallArgsEvent:
			args[ev.ToolCallID] += ev.Delta
		case *events.ToolCallEndEvent:
			card.progress(progressFor(names[ev.ToolCallID], args[ev.ToolCallID]))
		}
	}
}

func progressFor(name, rawArgs string) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(rawArgs), &a)
	if name == progressTool {
		if m, ok := a["message"].(string); ok {
			return m
		}
	}
	for _, v := range a {
		if s, ok := v.(string); ok {
			if len(s) > 60 {
				s = s[:60] + "…"
			}
			return name + ": " + s
		}
	}
	return name
}
