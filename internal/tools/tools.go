// Package tools holds the function tools that do work in the world. Each is
// a plain function wrapped with adk's functiontool; schemas come from struct
// tags. The harness tools live in the builtin subpackage.
package tools

import (
	"fmt"

	"google.golang.org/adk/v2/tool"
)

// World builds every world tool, keyed by name.
func World() (map[string]tool.Tool, error) {
	out := map[string]tool.Tool{}
	for _, build := range []func() (tool.Tool, error){calculate, webFetch, writeNote} {
		t, err := build()
		if err != nil {
			return nil, fmt.Errorf("tool: %w", err)
		}
		out[t.Name()] = t
	}
	return out, nil
}
