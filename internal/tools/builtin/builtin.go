// Package builtin holds the harness tools: the ones that drive the run
// itself (sub-agent dispatch, progress, questions) or feed the UI (task lists,
// artifacts), as opposed to tools that do work in the world.
package builtin

import (
	"context"
	"fmt"
	"iter"

	"github.com/rs/zerolog"

	"agentlite/internal/artifacts"
	"agentlite/internal/config"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/pkg/toolx"
)

// Tool names other packages key on.
const (
	Subagent = "subagent" // sub-agent dispatch; offered only to agents with subagents
	Question = "question" // answered by the user, not approved
)

// Dispatcher starts, follows and stops child runs; runs.Manager implements it.
type Dispatcher interface {
	Start(ctx context.Context, in runs.StartInput) (*runs.Run, error)
	Wait(ctx context.Context, runID string) (*runs.Run, error)
	Subscribe(ctx context.Context, threadID, after string) (iter.Seq2[runs.Entry, error], error)
	Cancel(ctx context.Context, threadID string) error
}

// Deps are shared by the builtin tools.
type Deps struct {
	Dispatcher Dispatcher
	Defs       map[string]config.AgentDef
	Artifacts  *artifacts.Store
	Skills     *skills.Library // nil: no write_skill
	Log        zerolog.Logger
}

// All builds every builtin tool, keyed by name.
func All(d Deps) (map[string]toolx.Tool, error) {
	builders := []func(Deps) (toolx.Tool, error){subagent, todoWrite, question, reportProgress, emitArtifact}
	if d.Skills != nil {
		builders = append(builders, writeSkill)
	}
	out := make(map[string]toolx.Tool, len(builders))
	for _, build := range builders {
		t, err := build(d)
		if err != nil {
			return nil, fmt.Errorf("builtin tool: %w", err)
		}
		out[t.Name()] = t
	}
	return out, nil
}
