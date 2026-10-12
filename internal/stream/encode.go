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
// deltas; tool calls, results and interrupts are emitted when adk reports
// them. Steps are kept per author because a parallel agent interleaves its
// sub-agents' events.
type Encoder struct {
	// ToolUI names the UI renderer of the tools that have one; it rides in
	// their TOOL_CALL_START metadata, so stored history renders the same.
	ToolUI map[string]string

	threadID, runID, model string
	emit                   func(Event)
	startedAt              time.Time

	steps      map[string]*step // open model calls by author
	result     string           // text of the last completed text block (the answer)
	shown      strings.Builder  // all text the segment showed
	calls      map[string]bool  // tool call ids already emitted
	usage      events.TokenUsage
	interrupts []types.Interrupt
}

// step is one model call in progress: its open text and reasoning blocks.
type step struct {
	text, reasoning string // open message ids
	buf             strings.Builder
	streamed        bool // text arrived as partial deltas; the final event repeats it
}

// NewEncoder prepares an encoder; Start emits RUN_STARTED.
func NewEncoder(threadID, runID, model string, emit func(Event)) *Encoder {
	return &Encoder{
		threadID: threadID, runID: runID, model: model, emit: emit,
		steps: map[string]*step{}, calls: map[string]bool{}, usage: events.TokenUsage{Model: model},
	}
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
		e.results(ev)
		return
	}
	s := e.steps[ev.Author]
	if s == nil {
		s = &step{}
		e.steps[ev.Author] = s
		e.send(events.NewStepStartedEvent(ev.Author))
	}
	for _, p := range ev.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			e.closeBlocks(s)
			e.call(ev, p.FunctionCall)
		case p.Text != "" && ev.Partial:
			s.streamed = true
			e.delta(s, p)
		case p.Text != "" && !s.streamed:
			e.delta(s, p)
		}
	}
	if !ev.Partial {
		e.closeStep(ev.Author)
	}
}

// Finish closes the segment. It returns the interrupts the run paused on, if any.
func (e *Encoder) Finish() []types.Interrupt {
	e.closeSteps()
	outcome := events.WithSuccessOutcome()
	if len(e.interrupts) > 0 {
		outcome = events.WithInterruptOutcome(e.interrupts)
	}
	e.finish(events.WithUsage([]events.TokenUsage{e.usage}), outcome)
	return e.interrupts
}

// Cancel closes the segment as cancelled.
func (e *Encoder) Cancel() {
	e.closeSteps()
	e.finish(events.WithOutcome(events.RunFinishedOutcome{Type: OutcomeCancelled}))
}

// Fail closes the segment with RUN_ERROR.
func (e *Encoder) Fail(err error) {
	e.closeSteps()
	e.send(events.NewRunErrorEvent(err.Error(), events.WithRunID(e.runID)))
}

// Result is the text of the last completed text block.
func (e *Encoder) Result() string { return e.result }

// Shown is all the text the segment showed, from every text block.
func (e *Encoder) Shown() string { return e.shown.String() }

func (e *Encoder) finish(opts ...events.RunFinishedOption) {
	ev := events.NewRunFinishedEventWithOptions(e.threadID, e.runID, opts...)
	ev.GetBaseEvent().Metadata = types.Metadata{MetaDurationMs: time.Since(e.startedAt).Milliseconds()}
	e.send(ev)
}

func (e *Encoder) closeStep(author string) {
	s := e.steps[author]
	if s == nil {
		return
	}
	e.closeBlocks(s)
	e.send(events.NewStepFinishedEvent(author))
	delete(e.steps, author)
}

func (e *Encoder) closeSteps() {
	for author := range e.steps {
		e.closeStep(author)
	}
}

func (e *Encoder) delta(s *step, p *genai.Part) {
	if p.Thought {
		if s.reasoning == "" {
			e.closeText(s)
			s.reasoning = uuid.NewString()
			e.send(events.NewReasoningStartEvent(s.reasoning))
			e.send(events.NewReasoningMessageStartEvent(s.reasoning, "reasoning"))
		}
		e.send(events.NewReasoningMessageContentEvent(s.reasoning, p.Text))
		return
	}
	if s.text == "" {
		e.closeReasoning(s)
		s.text = uuid.NewString()
		s.buf.Reset()
		e.send(events.NewTextMessageStartEvent(s.text, events.WithRole("assistant")))
	}
	s.buf.WriteString(p.Text)
	e.shown.WriteString(p.Text)
	e.send(events.NewTextMessageContentEvent(s.text, p.Text))
}

func (e *Encoder) closeBlocks(s *step) {
	e.closeText(s)
	e.closeReasoning(s)
}

func (e *Encoder) closeText(s *step) {
	if s.text != "" {
		e.send(events.NewTextMessageEndEvent(s.text))
		e.result = s.buf.String()
		s.text = ""
	}
}

func (e *Encoder) closeReasoning(s *step) {
	if s.reasoning != "" {
		e.send(events.NewReasoningMessageEndEvent(s.reasoning))
		e.send(events.NewReasoningEndEvent(s.reasoning))
		s.reasoning = ""
	}
}

// call emits a tool call once; a confirmation wrapper records an interrupt so
// Finish pauses the run. Its request, when present, says what is asked: the
// questions of a question.
func (e *Encoder) call(ev *session.Event, fc *genai.FunctionCall) {
	if fc.Name == toolconfirmation.FunctionCallName {
		original, err := toolconfirmation.OriginalCallFrom(fc)
		if err != nil {
			return
		}
		e.toolCall(original)
		hint, payload := confirmation(fc.Args["toolConfirmation"])
		in := interrupt(fc.ID, ReasonConfirmation, original, hint)
		if payload != nil {
			in.Metadata["request"] = payload
		}
		e.interrupts = append(e.interrupts, in)
		return
	}
	e.toolCall(fc)
}

// interrupt describes a pause; the tool name and arguments ride in Metadata so
// clients and the hitl record can show what is being asked.
func interrupt(id, reason string, call *genai.FunctionCall, message string) types.Interrupt {
	return types.Interrupt{
		ID: id, Reason: reason, ToolCallID: call.ID, Message: message,
		Metadata: types.Metadata{"toolName": call.Name, "args": call.Args},
	}
}

// confirmation reads the hint and payload however adk shaped the
// confirmation: a struct when emitted in-process, a map after a JSON round trip.
func confirmation(v any) (string, any) {
	switch c := v.(type) {
	case toolconfirmation.ToolConfirmation:
		return c.Hint, c.Payload
	case *toolconfirmation.ToolConfirmation:
		return c.Hint, c.Payload
	case map[string]any:
		hint, _ := c["hint"].(string)
		return hint, c["payload"]
	}
	return "", nil
}

func (e *Encoder) toolCall(fc *genai.FunctionCall) {
	if e.calls[fc.ID] {
		return
	}
	e.calls[fc.ID] = true
	args, _ := json.Marshal(fc.Args)
	start := events.NewToolCallStartEvent(fc.ID, fc.Name)
	if ui := e.ToolUI[fc.Name]; ui != "" {
		start.GetBaseEvent().Metadata = types.Metadata{MetaUI: ui}
	}
	e.send(start)
	e.send(events.NewToolCallArgsEvent(fc.ID, string(args)))
	e.send(events.NewToolCallEndEvent(fc.ID))
}

// results emits tool results. adk's own bookkeeping is skipped: the answer to
// a confirmation wrapper, and the placeholder result it records on a call it
// is asking confirmation for.
func (e *Encoder) results(ev *session.Event) {
	for _, p := range ev.Content.Parts {
		fr := p.FunctionResponse
		if fr == nil || fr.Name == toolconfirmation.FunctionCallName {
			continue
		}
		if _, pending := ev.Actions.RequestedToolConfirmations[fr.ID]; pending {
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

func (e *Encoder) addUsage(u *genai.GenerateContentResponseUsageMetadata) {
	add(&e.usage.InputTokens, u.PromptTokenCount)
	add(&e.usage.OutputTokens, u.CandidatesTokenCount)
	add(&e.usage.ReasoningTokens, u.ThoughtsTokenCount)
	add(&e.usage.TotalTokens, u.TotalTokenCount)
}

func add(p **int64, n int32) {
	if *p == nil {
		*p = new(int64)
	}
	**p += int64(n)
}

func hasFunctionResponse(c *genai.Content) bool {
	return slices.ContainsFunc(c.Parts, func(p *genai.Part) bool { return p.FunctionResponse != nil })
}

// Activity builds a replace-snapshot activity event.
func Activity(messageID, activityType string, content any) Event {
	return events.NewActivitySnapshotEvent(messageID, activityType, content).WithReplace(true)
}
