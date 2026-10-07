package stream

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// Tool names the encoder turns into activities.
const (
	ToolTodoWrite    = "todowrite"
	ToolEmitArtifact = "emit_artifact"
)

// Encoder turns one run segment's adk events into AG-UI events. Every model
// call becomes a step named after its author; text and reasoning stream as
// deltas; tool calls, results and interrupts are emitted when adk reports them.
type Encoder struct {
	threadID, runID, model string
	emit                   func(Event)
	startedAt              time.Time

	step       string          // open step name
	text       string          // open text message id
	reasoning  string          // open reasoning message id
	streamed   bool            // the current model turn's text arrived as partial deltas
	result     string          // text of the last completed text block (the answer)
	calls      map[string]bool // tool call ids already emitted
	usage      events.TokenUsage
	interrupts []types.Interrupt
}

// NewEncoder prepares an encoder; Start emits RUN_STARTED.
func NewEncoder(threadID, runID, model string, emit func(Event)) *Encoder {
	return &Encoder{threadID: threadID, runID: runID, model: model, emit: emit, calls: map[string]bool{}, usage: events.TokenUsage{Model: model}}
}

func (e *Encoder) send(ev Event) {
	ev.SetTimestamp(time.Now().UnixMilli())
	e.emit(ev)
}

// Start emits RUN_STARTED with the turn metadata.
func (e *Encoder) Start(turnID, agentID string) {
	e.startedAt = time.Now()
	ev := events.NewRunStartedEvent(e.threadID, e.runID)
	ev.GetBaseEvent().Metadata = types.Metadata{
		MetaTurnID: turnID, MetaAgentID: agentID, MetaModel: e.model, MetaCreatedAt: e.startedAt.UnixMilli(),
	}
	e.send(ev)
}

// Handle translates one adk event.
func (e *Encoder) Handle(ev *session.Event) {
	if ev.UsageMetadata != nil && !ev.Partial {
		e.addUsage(ev.UsageMetadata)
	}
	if ev.Content == nil {
		return
	}
	if hasFunctionResponse(ev.Content) {
		e.results(ev.Content)
		return
	}
	e.openStep(ev.Author)
	for _, p := range ev.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			e.call(ev, p.FunctionCall)
		case p.Text != "" && ev.Partial:
			e.streamed = true
			e.delta(p)
		case p.Text != "" && !e.streamed:
			e.delta(p)
		}
	}
	if !ev.Partial {
		e.closeBlocks()
		e.closeStep()
		e.streamed = false
	}
}

// Finish closes the segment. It returns the interrupts the run paused on, if any.
func (e *Encoder) Finish() []types.Interrupt {
	e.closeBlocks()
	e.closeStep()
	ev := events.NewRunFinishedEventWithOptions(e.threadID, e.runID, events.WithUsage([]events.TokenUsage{e.usage}))
	if len(e.interrupts) > 0 {
		ev = events.NewRunFinishedEventWithOptions(e.threadID, e.runID, events.WithUsage([]events.TokenUsage{e.usage}), events.WithInterruptOutcome(e.interrupts))
	}
	ev.GetBaseEvent().Metadata = types.Metadata{MetaDurationMs: time.Since(e.startedAt).Milliseconds()}
	e.send(ev)
	return e.interrupts
}

// Cancel closes the segment as cancelled.
func (e *Encoder) Cancel() {
	e.closeBlocks()
	e.closeStep()
	ev := events.NewRunFinishedEventWithOptions(e.threadID, e.runID, events.WithOutcome(events.RunFinishedOutcome{Type: events.RunFinishedOutcomeType("cancelled")}))
	ev.GetBaseEvent().Metadata = types.Metadata{MetaDurationMs: time.Since(e.startedAt).Milliseconds()}
	e.send(ev)
}

// Fail closes the segment with RUN_ERROR.
func (e *Encoder) Fail(err error) {
	e.closeBlocks()
	e.closeStep()
	e.send(events.NewRunErrorEvent(err.Error(), events.WithRunID(e.runID)))
}

// Result is the text of the last completed text block.
func (e *Encoder) Result() string { return e.result }

func (e *Encoder) openStep(name string) {
	if e.step == name {
		return
	}
	e.closeStep()
	e.step = name
	e.send(events.NewStepStartedEvent(name))
}

func (e *Encoder) closeStep() {
	if e.step == "" {
		return
	}
	e.send(events.NewStepFinishedEvent(e.step))
	e.step = ""
}

func (e *Encoder) delta(p *genai.Part) {
	if p.Thought {
		if e.reasoning == "" {
			e.closeText()
			e.reasoning = uuid.NewString()
			e.send(events.NewReasoningStartEvent(e.reasoning))
			e.send(events.NewReasoningMessageStartEvent(e.reasoning, "reasoning"))
		}
		e.send(events.NewReasoningMessageContentEvent(e.reasoning, p.Text))
		return
	}
	if e.text == "" {
		e.closeReasoning()
		e.text = uuid.NewString()
		e.result = ""
		e.send(events.NewTextMessageStartEvent(e.text, events.WithRole("assistant")))
	}
	e.result += p.Text
	e.send(events.NewTextMessageContentEvent(e.text, p.Text))
}

func (e *Encoder) closeBlocks() {
	e.closeText()
	e.closeReasoning()
}

func (e *Encoder) closeText() {
	if e.text != "" {
		e.send(events.NewTextMessageEndEvent(e.text))
		e.text = ""
	}
}

func (e *Encoder) closeReasoning() {
	if e.reasoning != "" {
		e.send(events.NewReasoningMessageEndEvent(e.reasoning))
		e.send(events.NewReasoningEndEvent(e.reasoning))
		e.reasoning = ""
	}
}

// call emits a tool call once; confirmation wrappers and long-running tools
// record an interrupt so Finish pauses the run.
func (e *Encoder) call(ev *session.Event, fc *genai.FunctionCall) {
	e.closeBlocks()
	if fc.Name == toolconfirmation.FunctionCallName {
		original, err := toolconfirmation.OriginalCallFrom(fc)
		if err != nil {
			return
		}
		e.toolCall(original)
		e.interrupts = append(e.interrupts, types.Interrupt{ID: fc.ID, Reason: ReasonConfirmation, ToolCallID: original.ID, Message: confirmationHint(fc.Args["toolConfirmation"])})
		return
	}
	e.toolCall(fc)
	if slices.Contains(ev.LongRunningToolIDs, fc.ID) {
		e.interrupts = append(e.interrupts, types.Interrupt{ID: fc.ID, Reason: ReasonInputRequired, ToolCallID: fc.ID})
	}
}

// confirmationHint reads the hint however adk shaped the confirmation: a
// struct when emitted in-process, a map after a JSON round trip.
func confirmationHint(v any) string {
	switch c := v.(type) {
	case toolconfirmation.ToolConfirmation:
		return c.Hint
	case *toolconfirmation.ToolConfirmation:
		return c.Hint
	case map[string]any:
		hint, _ := c["hint"].(string)
		return hint
	}
	return ""
}

func (e *Encoder) toolCall(fc *genai.FunctionCall) {
	if e.calls[fc.ID] {
		return
	}
	e.calls[fc.ID] = true
	args, _ := json.Marshal(fc.Args)
	e.send(events.NewToolCallStartEvent(fc.ID, fc.Name))
	e.send(events.NewToolCallArgsEvent(fc.ID, string(args)))
	e.send(events.NewToolCallEndEvent(fc.ID))
}

// results emits tool results. adk's own bookkeeping is skipped: the answer
// to a confirmation wrapper, and the "requires confirmation" error it records
// on the real call before asking.
func (e *Encoder) results(c *genai.Content) {
	for _, p := range c.Parts {
		fr := p.FunctionResponse
		if fr == nil || fr.Name == toolconfirmation.FunctionCallName || confirmationPending(fr) {
			continue
		}
		body, _ := json.Marshal(fr.Response)
		e.send(events.NewToolCallResultEvent(uuid.NewString(), fr.ID, string(body)))
		switch fr.Name {
		case ToolTodoWrite:
			e.send(Activity("tasks", ActivityTaskList, fr.Response))
		case ToolEmitArtifact:
			if id, ok := fr.Response["id"].(string); ok {
				e.send(Activity(id, ActivityArtifact, fr.Response))
			}
		}
	}
}

func confirmationPending(fr *genai.FunctionResponse) bool {
	msg, _ := fr.Response["error"].(string)
	return msg != "" && strings.Contains(msg, tool.ErrConfirmationRequired.Error())
}

func (e *Encoder) addUsage(u *genai.GenerateContentResponseUsageMetadata) {
	in := int64(u.PromptTokenCount) + valueOf(e.usage.InputTokens)
	out := int64(u.CandidatesTokenCount+u.ThoughtsTokenCount) + valueOf(e.usage.OutputTokens)
	e.usage.InputTokens, e.usage.OutputTokens = &in, &out
}

func valueOf(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func hasFunctionResponse(c *genai.Content) bool {
	return slices.ContainsFunc(c.Parts, func(p *genai.Part) bool { return p.FunctionResponse != nil })
}

// Activity builds a replace-snapshot activity event.
func Activity(messageID, activityType string, content any) Event {
	ev := events.NewActivitySnapshotEvent(messageID, activityType, content)
	replace := true
	ev.Replace = &replace
	return ev
}
