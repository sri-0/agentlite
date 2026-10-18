package agents

import (
	"slices"
	"testing"

	"google.golang.org/adk/v2/tool"

	"agentlite/config"
	"agentlite/internal/runs"
	"agentlite/pkg/toolx"
)

// TestSwitchable: generate_image is the "images" connector, so a run that
// switches it off doesn't get the tool, one that keeps it does, and an agent
// that lists it by name gets it either way; search_userfiles wasn't built,
// so there is no "files" connector.
func TestSwitchable(t *testing.T) {
	noop := func(toolx.Context, struct{}) (map[string]any, error) { return nil, nil }
	harness := map[string]toolx.Tool{}
	for _, name := range []string{"generate_image", "todowrite"} {
		tl, err := toolx.New(toolx.Spec{Name: name, Description: name}, noop)
		if err != nil {
			t.Fatal(err)
		}
		harness[name] = tl
	}
	rt := &Runtime{harness: harness, world: map[string]toolx.Tool{}, sets: map[string]tool.Toolset{}}
	if err := rt.switchable(); err != nil {
		t.Fatal(err)
	}
	if err := rt.CheckConnectors([]string{"images"}); err != nil {
		t.Errorf("images: %v", err)
	}
	if err := rt.CheckConnectors([]string{"files"}); err == nil {
		t.Error("files is a connector without search_userfiles")
	}

	every := config.AgentDef{Kind: config.KindLLM} // tools omitted: harness plus the run's connectors
	for _, tc := range []struct {
		name       string
		connectors *[]string
		images     bool
	}{
		{"all", nil, true},
		{"images on", &[]string{"images"}, true},
		{"images off", &[]string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fns, sets, err := rt.toolsFor(every, &runs.Run{ThreadID: "t", RootID: "t", Connectors: tc.connectors})
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(fns, func(f tool.Tool) bool { return f.Name() == "generate_image" }) {
				t.Error("generate_image is still a harness tool")
			}
			got := slices.ContainsFunc(sets, func(s tool.Toolset) bool { return s != nil && s.Name() == "images" })
			if got != tc.images {
				t.Errorf("images toolset offered: %v, want %v", got, tc.images)
			}
		})
	}

	listed := config.AgentDef{Kind: config.KindLLM, Tools: &[]string{"generate_image"}}
	fns, _, err := rt.toolsFor(listed, &runs.Run{ThreadID: "t", RootID: "t", Connectors: &[]string{}})
	if err != nil || len(fns) != 1 || fns[0].Name() != "generate_image" {
		t.Errorf("an agent listing generate_image gets %v (%v)", fns, err)
	}
}
