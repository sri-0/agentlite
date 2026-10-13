package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"agentlite/pkg/modelcatalog"
)

// ModelsConfig is config/default/models.yaml: the upstream providers and the
// models they serve. A model id is "<provider>/<model>": the provider's id,
// then the provider's own name for the model.
type ModelsConfig struct {
	Providers    []Provider    `yaml:"providers"`
	Memory       *MemoryConfig `yaml:"memory"`
	DefaultModel string        `yaml:"default_model"` // answers when a request names no model
	Compaction   *Compaction   `yaml:"compaction"`    // nil disables context compaction
}

// Compaction configures adk's session compaction for every run: a summary
// every Interval user turns, keeping Overlap turns for continuity, and a
// tail-retention pass when the prompt exceeds TokenThreshold of the model's
// context window.
type Compaction struct {
	Interval       int     `yaml:"interval"`
	Overlap        int     `yaml:"overlap"`
	TokenThreshold float64 `yaml:"token_threshold"`
}

// MemoryConfig enables long-term memory: which model embeds entries and the
// vector size it produces.
type MemoryConfig struct {
	EmbeddingModel string `yaml:"embedding_model"`
	Dimensions     int    `yaml:"dimensions"`
	// Preload searches memory before every model call of a user-started run;
	// off, the agent decides when to search with the load_memory tool.
	Preload bool `yaml:"preload"`
}

// Provider is one OpenAI-compatible upstream. Secrets are named by environment
// variable; base_url and header values may use ${ENV} expansion.
type Provider struct {
	ID        string            `yaml:"id"`
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	API       string            `yaml:"api"` // chat_completions (default) | responses
	Headers   map[string]string `yaml:"headers"`
	Discover  *Discover         `yaml:"discover"`
	// Models are declared by the provider's own id; each overrides, field by
	// field, what discovery and a model card say about that model, and may
	// set its own base_url and api.
	Models []modelcatalog.Model `yaml:"models"`
	TLS    *TLS                 `yaml:"tls"` // mutual TLS and trusted authorities, from env-named files
}

// Discover fetches the provider's model list and merges the YAML models over it.
type Discover struct {
	Endpoint string        `yaml:"endpoint"` // relative to base_url: /models or /model/info
	Refresh  time.Duration `yaml:"refresh"`
}

// LoadModels reads models.yaml and expands ${ENV} references.
func LoadModels(dir string) (*ModelsConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	var cfg ModelsConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	if cfg.DefaultModel == "" {
		return nil, fmt.Errorf("models.yaml: default_model is required")
	}
	if c := cfg.Compaction; c != nil && (c.Interval <= 0 || c.Overlap < 0 || c.Overlap >= c.Interval || c.TokenThreshold < 0 || c.TokenThreshold > 1) {
		return nil, fmt.Errorf("models.yaml: compaction needs interval > overlap >= 0 and token_threshold within 0..1")
	}
	seen := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		switch {
		case p.ID == "" || strings.Contains(p.ID, "/"):
			return nil, fmt.Errorf("models.yaml: provider id %q must be set and contain no /", p.ID)
		case seen[p.ID]:
			return nil, fmt.Errorf("models.yaml: duplicate provider %q", p.ID)
		}
		seen[p.ID] = true
		p.BaseURL = os.ExpandEnv(p.BaseURL)
		expandEnv(p.Headers)
		if p.Name == "" {
			p.Name = p.ID
		}
		for j := range p.Models {
			m := &p.Models[j]
			m.BaseURL = os.ExpandEnv(m.BaseURL)
			if _, err := url.Parse(m.BaseURL); err != nil {
				return nil, fmt.Errorf("models.yaml: model %s/%s: base_url: %w", p.ID, m.ID, err)
			}
		}
	}
	// Strict routing starts here: a model id must name a provider.
	named := map[string]string{"default_model": cfg.DefaultModel}
	if cfg.Memory != nil {
		named["memory.embedding_model"] = cfg.Memory.EmbeddingModel
	}
	for field, id := range named {
		if pid, _, _ := strings.Cut(id, "/"); !seen[pid] {
			return nil, fmt.Errorf("models.yaml: %s %q must start with a provider id", field, id)
		}
	}
	return &cfg, nil
}
