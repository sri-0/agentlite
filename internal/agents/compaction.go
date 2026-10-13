package agents

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"

	"agentlite/internal/stream"
)

// compaction builds adk's compaction config for a run from the global
// settings, summarising with the run's own model, and reports every
// compaction to the watch. Nil when compaction is off, or when only tail
// retention is configured and the model's context window is unknown.
func (rt *Runtime) compaction(ctx context.Context, modelID string, watch *compactionWatch) (*compaction.Config, error) {
	c := rt.compact
	if c == nil {
		return nil, nil
	}
	t, err := rt.registry.Resolve(modelID)
	if err != nil {
		return nil, err
	}
	cfg := &compaction.Config{}
	if window := t.Model.ContextLength; c.TokenThreshold > 0 && window > 0 { // the threshold needs a known window
		cfg.TokenThreshold, cfg.EventRetentionSize = int(c.TokenThreshold*float64(window)), c.KeepEvents
	}
	if c.Interval > 0 {
		cfg.CompactionInterval, cfg.OverlapSize = c.Interval, c.Overlap
	}
	if cfg.TokenThreshold == 0 && cfg.CompactionInterval == 0 {
		return nil, nil
	}
	llm, err := rt.llm(ctx, t)
	if err != nil {
		return nil, err
	}
	summarizer, err := compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{
		Model: llm, Timeout: c.SummarizerTimeout,
	})
	if err != nil {
		return nil, err
	}
	cfg.Summarizer = watchedSummarizer{Summarizer: summarizer, watch: watch}
	return cfg, nil
}

// compactionWatch shows a run's compactions in its stream as a compaction
// activity: running, then completed with the summary, failed, or declined
// (nothing to summarise, or adk discarded the summary). adk compacts without
// telling the runner's caller (it does not yield the event it records), so
// the run wraps the two things a compaction passes through: the summarizer,
// which starts it and may fail or decline it, and the session service, which
// receives the summary record. adk compacts between the events it yields: a
// node waits until the run's consumer has handled each event before it goes
// on, so nothing of a compaction overlaps the handling of an event.
type compactionWatch struct {
	sink stream.Sink

	mu      sync.Mutex
	id      string // the compaction in progress, "" when none
	started time.Time
	written bool // its summary is written; adk stores it next or discards it
}

// compactionActivity is the activity's content.
type compactionActivity struct {
	Status       string `json:"status"` // running | completed | failed | declined
	Summary      string `json:"summary,omitempty"`
	InputTokens  int32  `json:"inputTokens,omitempty"`
	OutputTokens int32  `json:"outputTokens,omitempty"`
	DurationMs   int64  `json:"durationMs,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (w *compactionWatch) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settle()
	w.id, w.started, w.written = "compaction:"+uuid.NewString(), time.Now(), false
	w.emit(compactionActivity{Status: "running"})
}

// summarized records that the summary is written.
func (w *compactionWatch) summarized() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = true
}

// end closes the compaction in progress with its outcome. A record stored
// with none in progress is adk correcting one already reported, and is not
// shown again.
func (w *compactionWatch) end(a compactionActivity) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finish(a)
}

// fail closes the compaction in progress, if any, as failed.
func (w *compactionWatch) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finish(compactionActivity{Status: "failed", Error: err.Error()})
}

// next is called for every event the run yields. A summary still open then
// was discarded: a stored one has been reported by the time adk yields.
func (w *compactionWatch) next() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.written {
		w.finish(compactionActivity{Status: "declined"})
	}
}

// close settles a compaction still open when the run ends.
func (w *compactionWatch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settle()
}

// settle closes a compaction left open: adk discarded its written summary
// (the session changed while it was written), or the run ended first.
func (w *compactionWatch) settle() {
	if w.written {
		w.finish(compactionActivity{Status: "declined"})
	} else {
		w.finish(compactionActivity{Status: "failed", Error: "the run ended before compaction finished"})
	}
}

func (w *compactionWatch) finish(a compactionActivity) {
	if w.id == "" {
		return
	}
	a.DurationMs = time.Since(w.started).Milliseconds()
	w.emit(a)
	w.id, w.written = "", false
}

func (w *compactionWatch) emit(a compactionActivity) {
	w.sink.Emit(stream.Activity(w.id, stream.ActivityCompaction, a))
}

// watchedSummarizer reports each summarisation's start, and its failure or
// decline; a success is reported when its record is stored.
type watchedSummarizer struct {
	compaction.Summarizer
	watch *compactionWatch
}

func (s watchedSummarizer) SummarizeEvents(
	ctx context.Context, events []*session.Event,
) (compaction.SummarizeResult, error) {
	s.watch.start()
	res, err := s.Summarizer.SummarizeEvents(ctx, events)
	switch {
	case err != nil:
		s.watch.end(compactionActivity{Status: "failed", Error: err.Error()})
	case res.Content == nil:
		s.watch.end(compactionActivity{Status: "declined"})
	default:
		s.watch.summarized()
	}
	return res, err
}

// watchedSessions passes every call through to the session service and
// reports the outcome of storing a compaction record.
type watchedSessions struct {
	session.Service
	watch *compactionWatch
}

func (s watchedSessions) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	err := s.Service.AppendEvent(ctx, sess, ev)
	c := ev.Actions.Compaction
	switch {
	case c == nil:
	case err != nil:
		s.watch.end(compactionActivity{Status: "failed", Error: err.Error()})
	default:
		done := compactionActivity{Status: "completed", Summary: stream.TextOf(c.CompactedContent)}
		if u := ev.UsageMetadata; u != nil {
			done.InputTokens, done.OutputTokens = u.PromptTokenCount, u.CandidatesTokenCount
		}
		s.watch.end(done)
	}
	return err
}
