package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-yaml"
)

// ModelsConfig is config/default/models.yaml: the upstream providers, the
// models they serve, and which provider answers the pass-through routes.
type ModelsConfig struct {
	Providers []Provider    `yaml:"providers"`
	Proxy     Proxy         `yaml:"proxy"`
	Memory    *MemoryConfig `yaml:"memory"`
}

// MemoryConfig enables long-term memory: which model embeds entries and the
// vector size it produces.
type MemoryConfig struct {
	EmbeddingModel string `yaml:"embedding_model"`
	Dimensions     int    `yaml:"dimensions"`
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
	Models    []Model           `yaml:"models"`

	SSLClientCertEnv      string `yaml:"ssl_client_cert_env"`
	SSLClientKeyEnv       string `yaml:"ssl_client_key_env"`
	SSLTrustStoreEnv      string `yaml:"ssl_trust_store_env"`
	SSLInsecureSkipVerify bool   `yaml:"ssl_insecure_skip_verify"`
}

// Discover fetches the provider's model list and merges the YAML models over it.
type Discover struct {
	Endpoint string        `yaml:"endpoint"` // relative to base_url: /models or /model/info
	Refresh  time.Duration `yaml:"refresh"`
}

// Proxy names the upstream for the pass-through routes, with per-route overrides
// keyed by route name (chat_completions, responses, messages, embeddings).
type Proxy struct {
	Provider  string            `yaml:"provider"`
	Overrides map[string]string `yaml:"overrides"`
}

// Model is served by GET /v1/models in this shape (plus object: "model").
type Model struct {
	ID                  string        `yaml:"id" json:"id"`
	Name                string        `yaml:"name" json:"name,omitempty"`
	Type                string        `yaml:"type" json:"type"` // llm | embedding
	OwnedBy             string        `yaml:"owned_by" json:"owned_by,omitempty"`
	Description         string        `yaml:"description" json:"description,omitempty"`
	ContextLength       int           `yaml:"context_length" json:"context_length,omitempty"`
	Capabilities        []string      `yaml:"capabilities" json:"capabilities,omitempty"`
	SupportedParameters []string      `yaml:"supported_parameters" json:"supported_parameters,omitempty"`
	Architecture        *Architecture `yaml:"architecture" json:"architecture,omitempty"`

	ProviderID   string `yaml:"-" json:"provider_id"`
	ProviderName string `yaml:"-" json:"provider_name"`
}

// Architecture mirrors the OpenRouter model metadata shape.
type Architecture struct {
	InputModalities  []string `yaml:"input_modalities" json:"input_modalities,omitempty"`
	OutputModalities []string `yaml:"output_modalities" json:"output_modalities,omitempty"`
	Tokenizer        string   `yaml:"tokenizer" json:"tokenizer,omitempty"`
	InstructType     string   `yaml:"instruct_type" json:"instruct_type,omitempty"`
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
	cfg.Proxy.Provider = os.ExpandEnv(cfg.Proxy.Provider)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		p.BaseURL = os.ExpandEnv(p.BaseURL)
		for k, v := range p.Headers {
			p.Headers[k] = os.ExpandEnv(v)
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		for j := range p.Models {
			m := &p.Models[j]
			if m.Type == "" {
				m.Type = "llm"
			}
			m.ProviderID, m.ProviderName = p.ID, p.Name
		}
	}
	if cfg.Proxy.Provider == "" && len(cfg.Providers) > 0 {
		cfg.Proxy.Provider = cfg.Providers[0].ID
	}
	return &cfg, nil
}
