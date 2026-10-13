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

// Compaction configures adk's session compaction for every run. Tail
// retention is the main strategy: before a model call whose prompt passes
// TokenThreshold of the model's context window, everything but the newest
// KeepEvents events is summarised. A sliding window (a summary every Interval
// user turns, keeping Overlap of them) can run too; it runs after the answer.
type Compaction struct {
	TokenThreshold    float64       `yaml:"token_threshold"` // 0 turns tail retention off
	KeepEvents        int           `yaml:"keep_events"`     // default 20
	Interval          int           `yaml:"interval"`        // 0 turns the sliding window off
	Overlap           int           `yaml:"overlap"`
	SummarizerTimeout time.Duration `yaml:"summarizer_timeout"` // default 60s
}

// Compaction defaults.
const (
	defaultKeepEvents        = 20
	defaultSummarizerTimeout = 60 * time.Second
)

func (c *Compaction) check() error {
	if c.KeepEvents == 0 {
		c.KeepEvents = defaultKeepEvents
	}
	if c.SummarizerTimeout == 0 {
		c.SummarizerTimeout = defaultSummarizerTimeout
	}
	switch {
	case c.TokenThreshold < 0 || c.TokenThreshold > 1:
		return fmt.Errorf("token_threshold is a share of the context window, within 0..1")
	case c.TokenThreshold == 0 && c.Interval == 0:
		return fmt.Errorf("set token_threshold or interval, or remove the block")
	case c.Interval < 0 || c.Overlap < 0 || (c.Interval > 0 && c.Overlap >= c.Interval):
		return fmt.Errorf("interval > overlap >= 0")
	case c.KeepEvents < 1:
		return fmt.Errorf("keep_events is at least 1")
	}
	return nil
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
	if c := cfg.Compaction; c != nil {
		if err := c.check(); err != nil {
			return nil, fmt.Errorf("models.yaml: compaction: %w", err)
		}
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
