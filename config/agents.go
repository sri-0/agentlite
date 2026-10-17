package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrg/frontmatter"
)

// DefaultAgent answers when a run names no agent.
const DefaultAgent = "assistant"

// Agent kinds and modes.
const (
	KindLLM        = "llm"
	KindSequential = "sequential"

	ModePrimary  = "primary"
	ModeSubagent = "subagent"
	ModeAll      = "all"
)

// AgentDef is one config/default/agents/<id>.md file: YAML frontmatter plus the
// markdown body as the instruction. Agents reference each other by id.
type AgentDef struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
	Kind        string `yaml:"kind" json:"kind"`
	Mode        string `yaml:"mode" json:"mode"`

	// Tools is nil when omitted (every registered tool) and empty for none.
	Tools     *[]string `yaml:"tools" json:"tools,omitempty"`
	Subagents []string  `yaml:"subagents" json:"subagents,omitempty"`

	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`

	Instruction string `yaml:"-" json:"system_prompt,omitempty"`
}

// Primary reports whether the agent can be selected directly by a user.
func (a AgentDef) Primary() bool { return a.Mode == ModePrimary || a.Mode == ModeAll }

// loadAgents reads every agents/*.md file, sorted by file name (Glob's order).
func loadAgents(dir string) ([]AgentDef, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "agents", "*.md"))
	if err != nil {
		return nil, err
	}
	defs := make([]AgentDef, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		def, err := parseAgentFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if seen[def.ID] {
			return nil, fmt.Errorf("%s: duplicate agent id %q", p, def.ID)
		}
		seen[def.ID] = true
		defs = append(defs, def)
	}
	byID := map[string]AgentDef{}
	for _, d := range defs {
		if err := d.validate(seen); err != nil {
			return nil, fmt.Errorf("agent %q: %w", d.ID, err)
		}
		byID[d.ID] = d
	}
	if d, ok := byID[DefaultAgent]; !ok || !d.Primary() {
		return nil, fmt.Errorf("agents: the default agent %q must exist and be primary", DefaultAgent)
	}
	for _, d := range defs {
		if err := checkCycle(byID, d.ID, nil); err != nil {
			return nil, err
		}
	}
	return defs, nil
}

// checkCycle refuses a pipeline that contains itself: building it would
// never end. Only pipelines nest when built; an llm agent's subagents run as
// runs of their own.
func checkCycle(byID map[string]AgentDef, id string, path []string) error {
	path = append(slices.Clone(path), id)
	if slices.Contains(path[:len(path)-1], id) {
		return fmt.Errorf("agents: pipeline cycle %s", strings.Join(path, " → "))
	}
	d := byID[id]
	if d.Kind != KindSequential {
		return nil
	}
	for _, sub := range d.Subagents {
		if err := checkCycle(byID, sub, path); err != nil {
			return err
		}
	}
	return nil
}

// validate checks what can be known before a run: every reference resolves
// and every kind has what it needs.
func (a AgentDef) validate(known map[string]bool) error {
	switch a.Kind {
	case KindLLM, KindSequential:
	default:
		return fmt.Errorf("unknown kind %q", a.Kind)
	}
	switch a.Mode {
	case ModePrimary, ModeSubagent, ModeAll:
	default:
		return fmt.Errorf("unknown mode %q", a.Mode)
	}
	if a.Kind != KindLLM && len(a.Subagents) == 0 {
		return fmt.Errorf("kind %s needs subagents", a.Kind)
	}
	if a.Kind == KindLLM && len(a.Subagents) > 0 && a.Tools != nil && !slices.Contains(*a.Tools, "subagent") {
		return fmt.Errorf("subagents are unreachable without the subagent tool")
	}
	for _, sub := range a.Subagents {
		if !known[sub] {
			return fmt.Errorf("unknown subagent %q", sub)
		}
	}
	return nil
}

func parseAgentFile(path string) (AgentDef, error) {
	f, err := os.Open(path)
	if err != nil {
		return AgentDef{}, err
	}
	defer f.Close()
	var def AgentDef
	body, err := frontmatter.MustParse(f, &def, frontmatter.NewFormat("---", "---", decode))
	if err != nil {
		return AgentDef{}, fmt.Errorf("frontmatter: %w", err)
	}
	if def.ID == "" {
		return AgentDef{}, fmt.Errorf("missing id")
	}
	if def.Name == "" {
		def.Name = def.ID
	}
	if def.Kind == "" {
		def.Kind = KindLLM
	}
	if def.Mode == "" {
		def.Mode = ModePrimary
	}
	def.Instruction = strings.TrimSpace(string(body))
	return def, nil
}
