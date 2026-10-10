package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

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

// LoadAgents reads every agents/*.md file, sorted by file name.
func LoadAgents(dir string) ([]AgentDef, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "agents", "*.md"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
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
	for _, d := range defs {
		if err := d.validate(seen); err != nil {
			return nil, fmt.Errorf("agent %q: %w", d.ID, err)
		}
	}
	return defs, nil
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
	if a.Kind == KindLLM && len(a.Subagents) > 0 && a.Tools != nil && !slices.Contains(*a.Tools, "task") {
		return fmt.Errorf("subagents are unreachable without the task tool")
	}
	for _, sub := range a.Subagents {
		if !known[sub] {
			return fmt.Errorf("unknown subagent %q", sub)
		}
	}
	return nil
}

func parseAgentFile(path string) (AgentDef, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return AgentDef{}, err
	}
	front, body, ok := strings.Cut(strings.TrimPrefix(string(raw), "---\n"), "\n---\n")
	if !ok || !strings.HasPrefix(string(raw), "---\n") {
		return AgentDef{}, fmt.Errorf("missing frontmatter")
	}
	var def AgentDef
	if err := yaml.Unmarshal([]byte(front), &def); err != nil {
		return AgentDef{}, err
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
	def.Instruction = strings.TrimSpace(body)
	return def, nil
}
