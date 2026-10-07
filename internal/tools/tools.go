// Package tools holds the function tools agents can use. Each tool is a plain
// function wrapped with adk's functiontool; schemas come from struct tags.
package tools

import (
	"context"
	"fmt"
	"iter"

	"google.golang.org/adk/v2/tool"

	"agentlite/internal/agent"
	"agentlite/internal/config"
)

// Tool names other packages refer to.
const (
	Task           = "task"
	ReportProgress = "report_progress"
)

// Dispatcher starts and follows child runs; the run service implements it.
type Dispatcher interface {
	Start(ctx context.Context, in agent.StartInput) (*agent.Run, error)
	Wait(ctx context.Context, runID string) (*agent.Run, error)
	Subscribe(ctx context.Context, threadID, after string) (iter.Seq2[agent.Entry, error], error)
}

// Deps are shared by every tool.
type Deps struct {
	Dispatcher Dispatcher
	Defs       map[string]config.AgentDef
}

// All builds every function tool, keyed by name.
func All(d Deps) (map[string]tool.Tool, error) {
	builders := []func(Deps) (tool.Tool, error){
		calculate, readURL, todoWrite, emitArtifact, writeNote, question, reportProgress, task,
	}
	out := make(map[string]tool.Tool, len(builders))
	for _, build := range builders {
		t, err := build(d)
		if err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		out[t.Name()] = t
	}
	return out, nil
}
