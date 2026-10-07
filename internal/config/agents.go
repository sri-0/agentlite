package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Agent kinds and modes.
const (
	KindLLM        = "llm"
	KindSequential = "sequential"
	KindParallel   = "parallel"
	KindLoop       = "loop"
	KindGraph      = "graph"

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
	Model       string `yaml:"model" json:"model,omitempty"`
	Provider    string `yaml:"provider" json:"provider,omitempty"`

	// Tools is nil when omitted (every registered tool) and empty for none.
	Tools     *[]string `yaml:"tools" json:"tools,omitempty"`
	Subagents []string  `yaml:"subagents" json:"subagents,omitempty"`
	Edges     []Edge    `yaml:"edges" json:"edges,omitempty"`

	MaxIterations   int         `yaml:"max_iterations" json:"max_iterations,omitempty"`
	MaxOutputTokens int         `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
	OutputKey       string      `yaml:"output_key" json:"output_key,omitempty"`
	Compaction      *Compaction `yaml:"compaction" json:"compaction,omitempty"`

	Instruction string `yaml:"-" json:"system_prompt,omitempty"`
}

// Edge is one connection of a graph agent; Route names a state key whose value
// selects the edge when several leave the same node.
type Edge struct {
	From  string `yaml:"from" json:"from"`
	To    string `yaml:"to" json:"to"`
	Route string `yaml:"route" json:"route,omitempty"`
}

// Compaction configures adk's session compaction for an agent: a summary
// every Interval user turns, keeping Overlap turns for continuity, and a
// tail-retention pass when the prompt exceeds TokenThreshold of the model's
// context window.
type Compaction struct {
	Interval       int     `yaml:"interval" json:"interval,omitempty"`
	Overlap        int     `yaml:"overlap" json:"overlap,omitempty"`
	TokenThreshold float64 `yaml:"token_threshold" json:"token_threshold,omitempty"`
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
		for _, sub := range d.Subagents {
			if !seen[sub] {
				return nil, fmt.Errorf("agent %q: unknown subagent %q", d.ID, sub)
			}
		}
	}
	return defs, nil
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
