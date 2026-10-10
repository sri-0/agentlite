// Package builtin holds the harness tools: the ones that drive the run
// itself (sub-agent dispatch, progress, questions) or feed the UI (task lists,
// artifacts), as opposed to tools that do work in the world.
package builtin

import (
	"context"
	"fmt"
	"iter"

	"google.golang.org/adk/v2/tool"

	"agentlite/internal/agent"
	"agentlite/internal/config"
)

// Task is the sub-agent dispatch tool; the builder keys its availability on it.
const Task = "task"

// Dispatcher starts, follows and stops child runs; the run service implements it.
type Dispatcher interface {
	Start(ctx context.Context, in agent.StartInput) (*agent.Run, error)
	Wait(ctx context.Context, runID string) (*agent.Run, error)
	Subscribe(ctx context.Context, threadID, after string) (iter.Seq2[agent.Entry, error], error)
	Cancel(ctx context.Context, threadID string) error
}

// Deps are shared by the builtin tools.
type Deps struct {
	Dispatcher Dispatcher
	Defs       map[string]config.AgentDef
}

// All builds every builtin tool, keyed by name.
func All(d Deps) (map[string]tool.Tool, error) {
	builders := []func(Deps) (tool.Tool, error){task, todoWrite, question, reportProgress, emitArtifact}
	out := make(map[string]tool.Tool, len(builders))
	for _, build := range builders {
		t, err := build(d)
		if err != nil {
			return nil, fmt.Errorf("builtin tool: %w", err)
		}
		out[t.Name()] = t
	}
	return out, nil
}
