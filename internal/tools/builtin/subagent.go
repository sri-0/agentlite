package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/google/uuid"

	"agentlite/internal/runs"
	"agentlite/internal/stream"
	"agentlite/pkg/toolx"
)

const (
	progressKeep = 10
)

type subagentArgs struct {
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
	// Interrupts are the requests the paused child waits on (status "waiting").
	Interrupts []types.Interrupt `json:"interrupts,omitempty"`
}

type progressLine struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}

// subagent runs a sub-agent on its own child thread and returns its answer.
// The child's activity stays on the child thread; the parent gets a card with
// status, a bounded progress log, and, while the child is paused, the
// requests it is waiting on, which the user answers on the child's thread.
// The call itself just waits for the child to finish, as opencode's does.
func subagent(d Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name:        Subagent,
		Description: "Delegate a self-contained task to a sub-agent and get its result. Several subagent calls in one turn run in parallel.",
	}, func(ctx toolx.Context, a subagentArgs) (map[string]any, error) {
		parent := d.Defs[ctx.AgentName()]
		if !slices.Contains(parent.Subagents, a.SubagentType) {
			return nil, fmt.Errorf("unknown subagent_type %q; choose one of %v", a.SubagentType, parent.Subagents)
		}
		sink, parentRun := stream.SinkFrom(ctx), runs.RunFrom(ctx)
		if sink == nil || parentRun == nil {
			return nil, fmt.Errorf("subagent tool needs a run context")
		}
		childThread := uuid.NewString()
		run, err := d.Dispatcher.Start(ctx, runs.StartInput{
			ThreadID: childThread, ParentID: ctx.SessionID(), AgentID: a.SubagentType, Model: parentRun.Model,
			Connectors: parentRun.Connectors, MessageID: uuid.NewString(), Text: a.Prompt,
			Authorization: toolx.RunFrom(ctx).Authorization, // the child acts for the same user
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

		followCtx, stopFollow := context.WithCancel(ctx)
		followed := make(chan struct{})
		go func() {
			defer close(followed)
			follow(followCtx, d.Dispatcher, childThread, card)
		}()
		final, err := d.Dispatcher.Wait(ctx, run.ID)
		stopFollow()
		<-followed
		switch {
		case err != nil:
			// The user cancelled the parent: the child goes too. On a server
			// shutdown it stays, paused or queued, and can be answered later.
			if errors.Is(context.Cause(ctx), runs.ErrCancelled) {
				if err := d.Dispatcher.Cancel(context.WithoutCancel(ctx), childThread); err != nil && !errors.Is(err, runs.ErrNotFound) {
					d.Log.Error().Err(err).Str("thread", childThread).Msg("cancel sub-agent")
				}
			}
			card.finish("error", "", err.Error())
			sink.Emit(events.NewSubagentErrorEvent(run.ID, err.Error()))
			return nil, err
		case final.Status != runs.StatusDone:
			msg := final.Error
			if msg == "" {
				msg = string(final.Status)
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
	if c.finished() {
		return
	}
	c.card.Progress = message
	c.card.Log = append(c.card.Log, progressLine{At: time.Now().UnixMilli(), Message: message})
	if len(c.card.Log) > progressKeep {
		c.card.Log = c.card.Log[len(c.card.Log)-progressKeep:]
	}
	c.emit()
}

// waiting shows the child's pending requests, or clears them when it resumes.
func (c *cardState) waiting(interrupts []types.Interrupt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished() {
		return
	}
	c.card.Interrupts, c.card.Status = interrupts, "running"
	if len(interrupts) > 0 {
		c.card.Status = "waiting"
	}
	c.emit()
}

func (c *cardState) finished() bool { return c.card.Status == "done" || c.card.Status == "error" }

func (c *cardState) finish(status, text, errText string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.card.Status, c.card.Text, c.card.Error, c.card.Interrupts = status, text, errText, nil
	c.card.DurationMs = time.Now().UnixMilli() - c.card.StartedAt
	c.emit()
}

// follow reads the child's log while the call waits, across the child's
// segments: each tool call becomes a progress line, a pause puts the child's
// pending requests on the card, and the next segment clears them.
func follow(ctx context.Context, d Dispatcher, threadID string, card *cardState) {
	after := ""
	names, args := map[string]string{}, map[string]string{}
	for ctx.Err() == nil {
		entries, err := d.Subscribe(ctx, threadID, after)
		if err != nil || entries == nil {
			return
		}
		for e, err := range entries {
			if err != nil {
				return
			}
			after = e.Seq
			switch ev := e.Event.(type) {
			case *events.RunStartedEvent:
				card.waiting(nil)
			case *events.ToolCallStartEvent:
				names[ev.ToolCallID] = ev.ToolCallName
			case *events.ToolCallArgsEvent:
				args[ev.ToolCallID] += ev.Delta
			case *events.ToolCallEndEvent:
				card.progress(progressFor(names[ev.ToolCallID], args[ev.ToolCallID]))
			case *events.RunFinishedEvent:
				if ev.Outcome != nil && ev.Outcome.Type == events.RunFinishedOutcomeTypeInterrupt {
					card.waiting(ev.Outcome.Interrupts)
				}
			}
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
