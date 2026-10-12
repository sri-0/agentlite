// Package toolx is how agentlite builds function tools. Every tool is made
// with New, which wraps adk's functiontool: the tool's function receives the
// thread its call belongs to and can record the sources it returns, and the
// tool can name a UI renderer for its calls. Registries and toolsets accept
// only Tools, and a test keeps adk's functiontool out of every other package,
// so no tool skips the wrapper.
package toolx

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// Spec describes a tool.
type Spec struct {
	Name        string
	Description string
	// RequireConfirmation pauses the run for the user's approval before each call.
	RequireConfirmation bool
	// UI names the renderer the UI uses for the tool's calls; empty means the
	// plain tool line.
	UI string
}

// Tool is a function tool built with New. Its zero value is not usable.
type Tool struct {
	adk tool.Tool
	ui  string
}

// New builds a tool. The input and output types give the schemas, as with
// adk's functiontool: struct fields with json and jsonschema tags.
func New[In, Out any](spec Spec, fn func(Context, In) (Out, error)) (Tool, error) {
	t, err := functiontool.New(functiontool.Config{
		Name: spec.Name, Description: spec.Description, RequireConfirmation: spec.RequireConfirmation,
	}, func(ctx agent.Context, in In) (Out, error) {
		return fn(newContext(ctx, spec.Name), in)
	})
	if err != nil {
		return Tool{}, fmt.Errorf("tool %s: %w", spec.Name, err)
	}
	return Tool{adk: t, ui: spec.UI}, nil
}

// Name is the name the model calls the tool by.
func (t Tool) Name() string { return t.adk.Name() }

// Description is what the model reads about the tool.
func (t Tool) Description() string { return t.adk.Description() }

// UI is the renderer the tool asks for, or "".
func (t Tool) UI() string { return t.ui }

// ADK is the tool as adk runs it.
func (t Tool) ADK() tool.Tool { return t.adk }

// Toolset groups tools under one name for adk, so switching the group off
// removes all of them.
func Toolset(name string, tools []Tool) tool.Toolset {
	ts := toolset{name: name, tools: make([]tool.Tool, len(tools))}
	for i, t := range tools {
		ts.tools[i] = t.adk
	}
	return ts
}

type toolset struct {
	name  string
	tools []tool.Tool
}

func (t toolset) Name() string                                     { return t.name }
func (t toolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return t.tools, nil }
