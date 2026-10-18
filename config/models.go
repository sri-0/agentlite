package config

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	Voice        *VoiceConfig  `yaml:"voice"`         // nil disables the voice prompt
	Images       *ImagesConfig `yaml:"images"`        // nil disables image generation
	Files        *FilesConfig  `yaml:"files"`         // nil: uploaded files are kept, not searchable
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
		return errors.New("token_threshold is a share of the context window, within 0..1")
	case c.TokenThreshold == 0 && c.Interval == 0:
		return errors.New("set token_threshold or interval, or remove the block")
	case c.Interval < 0 || c.Overlap < 0 || (c.Interval > 0 && c.Overlap >= c.Interval):
		return errors.New("interval > overlap >= 0")
	case c.KeepEvents < 1:
		return errors.New("keep_events is at least 1")
	}
	return nil
}

// MemoryConfig enables long-term memory: which model embeds entries. The
// memories index is sized for its vectors (deploy/opensearch/memories.json).
type MemoryConfig struct {
	EmbeddingModel string `yaml:"embedding_model"`
	// Preload searches memory before every model call of a user-started run;
	// off, the agent decides when to search with the load_memory tool.
	Preload bool `yaml:"preload"`
}

// VoiceConfig enables the voice prompt: the transcription model that turns a
// recording into the text of a message (internal/voice).
type VoiceConfig struct {
	Model string `yaml:"model"`
}

// ImagesConfig enables image generation (internal/images): the image model
// the generate_image tool uses, and the Images page's default.
type ImagesConfig struct {
	Model string `yaml:"model"`
}

// FilesConfig makes uploaded files searchable (internal/files, docs/FILES.md):
// the multimodal model that embeds them, the size of its vectors (the
// file_chunks index is sized for it), and the request format its server
// speaks: deepinfra (an OpenAI-style input list whose items may be content
// parts) or vllm (vLLM's chat-style embeddings, one request per input).
type FilesConfig struct {
	EmbeddingModel string `yaml:"embedding_model"`
	Dimensions     int    `yaml:"dimensions"` // default 768
	Format         string `yaml:"format"`     // deepinfra (default) or vllm
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

// APIs a provider or model may name; "" is chat_completions.
var apis = []string{"", "chat_completions", "responses"}

// loadModels reads models.yaml and expands ${ENV} references.
func loadModels(dir string) (*ModelsConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	var cfg ModelsConfig
	if err := decode(raw, &cfg); err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	if cfg.DefaultModel == "" {
		return nil, errors.New("models.yaml: default_model is required")
	}
	if c := cfg.Compaction; c != nil {
		if err := c.check(); err != nil {
			return nil, fmt.Errorf("models.yaml: compaction: %w", err)
		}
	}
	if f := cfg.Files; f != nil {
		f.Dimensions = cmp.Or(f.Dimensions, 768)
		f.Format = cmp.Or(f.Format, "deepinfra")
		if f.Format != "deepinfra" && f.Format != "vllm" {
			return nil, fmt.Errorf("models.yaml: files: format %q is not deepinfra or vllm", f.Format)
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
		case !slices.Contains(apis, p.API):
			return nil, fmt.Errorf("models.yaml: provider %s: api %q is not chat_completions or responses", p.ID, p.API)
		}
		seen[p.ID] = true
		// A provider with no base_url here is skipped by the registry, and
		// its headers are not needed.
		if p.BaseURL = os.ExpandEnv(p.BaseURL); p.BaseURL != "" {
			if err := expandEnv(p.Headers); err != nil {
				return nil, fmt.Errorf("models.yaml: provider %s: %w", p.ID, err)
			}
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		for j := range p.Models {
			m := &p.Models[j]
			if !slices.Contains(apis, m.API) {
				return nil, fmt.Errorf("models.yaml: model %s/%s: api %q is not chat_completions or responses", p.ID, m.ID, m.API)
			}
			// Checked by the providers registry; one whose variable is unset
			// must not quietly become the provider's own URL.
			if raw := m.BaseURL; raw != "" {
				if m.BaseURL = os.ExpandEnv(raw); m.BaseURL == "" {
					return nil, fmt.Errorf("models.yaml: model %s/%s: base_url %s is empty in this environment", p.ID, m.ID, raw)
				}
			}
		}
	}
	return &cfg, nil
}
