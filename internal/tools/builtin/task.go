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
	"github.com/google/uuid"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"agentlite/internal/agent"
	"agentlite/internal/hitl"
	"agentlite/internal/stream"
)

const (
	progressKeep = 10
)

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

// pausedChild is a sub-agent waiting for the user, kept in the parent's
// session state while the parent itself is paused on its behalf.
type pausedChild struct {
	ThreadID    string `json:"thread_id"`
	RunID       string `json:"run_id"`
	InterruptID string `json:"interrupt_id"`
	StartedAt   int64  `json:"started_at"`
}

// task runs a sub-agent on its own child thread and returns its answer. The
// child's activity stays on the child thread; the parent only gets a card
// with status and a bounded progress log. When the child pauses for an
// approval or a question, the task call pauses the parent with adk's own
// confirmation, carrying the child's request; adk calls the tool again with
// the user's answer, which goes to the child, and the wait continues.
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
		key := "task:" + ctx.FunctionCallID()

		var child pausedChild
		if conf := ctx.ToolConfirmation(); conf != nil { // the user answered the child's request
			if err := stateValue(ctx, key, &child); err != nil {
				return nil, fmt.Errorf("resume %s: %w", a.SubagentType, err)
			}
			_, err := d.Dispatcher.Decide(ctx, child.ThreadID, child.InterruptID, decisionFor(conf))
			// Not paused or not pending: the user already answered on the child's own thread.
			if err != nil && !errors.Is(err, agent.ErrNotPaused) && !errors.Is(err, agent.ErrNotFound) {
				return nil, fmt.Errorf("answer %s: %w", a.SubagentType, err)
			}
		} else {
			child = pausedChild{ThreadID: uuid.Must(uuid.NewV7()).String(), StartedAt: time.Now().UnixMilli()}
			run, err := d.Dispatcher.Start(ctx, agent.StartInput{
				ThreadID: child.ThreadID, ParentID: ctx.SessionID(), AgentID: a.SubagentType, Model: parentRun.Model,
				Connectors: parentRun.Connectors, MessageID: uuid.NewString(), Text: a.Prompt,
			})
			if err != nil {
				return nil, fmt.Errorf("start %s: %w", a.SubagentType, err)
			}
			child.RunID = run.ID
			sink.Emit(events.NewSubagentStartedEvent(run.ID, a.SubagentType, events.WithSubagentDescription(a.Description), events.WithParentToolCall(ctx.FunctionCallID(), "")))
		}
		card := &cardState{card: agentCard{
			SubagentRunID: child.RunID, ChildThreadID: child.ThreadID, Name: a.SubagentType, Description: a.Description,
			ParentToolCallID: ctx.FunctionCallID(), Status: "running", StartedAt: child.StartedAt,
		}, sink: sink}
		card.emit()

		tailCtx, stopTail := context.WithCancel(ctx)
		go tailProgress(tailCtx, d.Dispatcher, child.ThreadID, card)
		final, err := d.Dispatcher.Wait(ctx, child.RunID)
		stopTail()
		switch {
		case err != nil: // the parent was cancelled or Valkey failed: the child must not run on unseen
			_ = d.Dispatcher.Cancel(context.WithoutCancel(ctx), child.ThreadID)
			card.finish("error", "", err.Error())
			sink.Emit(events.NewSubagentErrorEvent(child.RunID, err.Error()))
			return nil, err
		case final.Status == agent.StatusAwaitingInput:
			return nil, forward(ctx, d, a.SubagentType, key, child, card)
		case final.Status != agent.StatusDone:
			msg := final.Error
			if msg == "" {
				msg = string(final.Status)
			}
			card.finish("error", final.Result, msg)
			sink.Emit(events.NewSubagentErrorEvent(child.RunID, msg))
			return nil, fmt.Errorf("%s %s: %s", a.SubagentType, final.Status, msg)
		}
		card.finish("done", final.Result, "")
		sink.Emit(events.NewSubagentFinishedEvent(child.RunID, events.WithSubagentSuccessOutcome(), events.WithSubagentResult(final.Result)))
		return map[string]any{"result": final.Result}, nil
	})
}

// forward pauses the parent on the child's pending request: the child is
// remembered in state, and the confirmation carries what the child asks.
func forward(ctx adkagent.Context, d Deps, name, key string, child pausedChild, card *cardState) error {
	pending, err := d.Dispatcher.Interrupts(ctx, child.ThreadID)
	if err != nil || len(pending) == 0 {
		_ = d.Dispatcher.Cancel(context.WithoutCancel(ctx), child.ThreadID)
		card.finish("error", "", "paused without a request")
		return fmt.Errorf("%s paused without a request: %v", name, err)
	}
	in := pending[0]
	child.InterruptID = in.ID
	if err := ctx.State().Set(key, child); err != nil {
		return err
	}
	card.progress("waiting for your answer: " + in.ToolName)
	args, _ := json.Marshal(in.Args)
	hint := fmt.Sprintf("%s wants to run %s %s", name, in.ToolName, args)
	if in.ToolName == "question" {
		hint = name + " asks you a question."
	}
	request := map[string]any{"subagent": name, "childThreadId": child.ThreadID, "toolName": in.ToolName, "args": in.Args}
	if err := ctx.RequestConfirmation(hint, request); err != nil {
		return err
	}
	return fmt.Errorf("%s: %w", name, tool.ErrConfirmationRequired)
}

// decisionFor turns the user's answer to a forwarded request into the
// child's decision: the answers to a question, or an approval.
func decisionFor(conf *toolconfirmation.ToolConfirmation) hitl.Decision {
	payload, _ := conf.Payload.(map[string]any)
	if out := payload["output"]; out != nil {
		return hitl.Decision{Output: out}
	}
	approved, _ := payload["approved"].(bool)
	justification, _ := payload["justification"].(string)
	return hitl.Decision{Approved: &approved, Justification: justification}
}

// stateValue reads a value written by an earlier call of this tool; after a
// round trip through the session store it comes back as a map.
func stateValue(ctx adkagent.Context, key string, out any) error {
	v, err := ctx.State().Get(key)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
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
