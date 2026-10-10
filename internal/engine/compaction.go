package engine

import (
	"context"

	"google.golang.org/adk/v2/session/compaction"
)

// retainedEvents is how many recent events a token-threshold compaction keeps
// verbatim so the turn being answered is never summarised away.
const retainedEvents = 20

// compaction builds adk's compaction config for a run from the global
// settings, summarising with the run's own model. Nil when compaction is off.
func (e *Engine) compaction(ctx context.Context, modelID string) (*compaction.Config, error) {
	if e.compact == nil {
		return nil, nil
	}
	llm, meta, err := e.model(ctx, modelID)
	if err != nil {
		return nil, err
	}
	summarizer, err := compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{Model: llm})
	if err != nil {
		return nil, err
	}
	cfg := &compaction.Config{CompactionInterval: e.compact.Interval, OverlapSize: e.compact.Overlap, Summarizer: summarizer}
	if e.compact.TokenThreshold > 0 && meta.ContextLength > 0 { // the threshold needs a known window
		cfg.TokenThreshold = int(e.compact.TokenThreshold * float64(meta.ContextLength))
		cfg.EventRetentionSize = retainedEvents
	}
	return cfg, nil
}
