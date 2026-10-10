package proxy

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"agentlite/internal/config"
)

// Pass-through route names, as used in models.yaml proxy.overrides.
const (
	RouteChatCompletions = "chat_completions"
	RouteResponses       = "responses"
	RouteMessages        = "messages"
	RouteEmbeddings      = "embeddings"
)

// Registry holds every provider client and the merged model list.
type Registry struct {
	cfg     config.Proxy
	clients []*Client // in models.yaml order
	log     zerolog.Logger

	mu         sync.RWMutex
	models     []config.Model
	discovered map[string][]config.Model // per provider, from its last successful discovery
}

// NewRegistry builds a client per provider and seeds the model list from YAML.
func NewRegistry(cfg *config.ModelsConfig, log zerolog.Logger) (*Registry, error) {
	r := &Registry{cfg: cfg.Proxy, log: log, discovered: map[string][]config.Model{}}
	for _, p := range cfg.Providers {
		if p.BaseURL == "" {
			log.Warn().Str("provider", p.ID).Msg("provider has no base_url in this environment; skipped")
			continue
		}
		c, err := NewClient(p)
		if err != nil {
			return nil, err
		}
		r.clients = append(r.clients, c)
	}
	for _, route := range []string{RouteChatCompletions, RouteResponses, RouteMessages, RouteEmbeddings} {
		if _, err := r.ProxyFor(route); err != nil {
			return nil, err
		}
	}
	r.rebuildModels("", nil)
	return r, nil
}

// Client returns the provider with the given id, or nil.
func (r *Registry) Client(id string) *Client {
	for _, c := range r.clients {
		if c.Config.ID == id {
			return c
		}
	}
	return nil
}

// ProxyFor resolves the upstream for a pass-through route.
func (r *Registry) ProxyFor(route string) (*Client, error) {
	id := r.cfg.Provider
	if o, ok := r.cfg.Overrides[route]; ok {
		id = o
	}
	c := r.Client(id)
	if c == nil {
		return nil, fmt.Errorf("proxy: unknown provider %q for route %s", id, route)
	}
	return c, nil
}

// Models returns the merged model list across providers.
func (r *Registry) Models() []config.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.models)
}

// Model resolves a model id to its provider and metadata; the first provider
// in models.yaml order that serves the id wins.
func (r *Registry) Model(modelID string) (*Client, *config.Model, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.models {
		m := &r.models[i]
		if m.ID == modelID {
			return r.Client(m.ProviderID), m, nil
		}
	}
	return nil, nil, fmt.Errorf("unknown model %q", modelID)
}

// Start runs model discovery for every provider that configures it, once now
// and then on each provider's refresh interval, until ctx ends.
func (r *Registry) Start(ctx context.Context) {
	for _, c := range r.clients {
		if c.Config.Discover != nil {
			go r.discoverLoop(ctx, c)
		}
	}
}

func (r *Registry) discoverLoop(ctx context.Context, c *Client) {
	refresh := c.Config.Discover.Refresh
	if refresh <= 0 {
		refresh = 10 * time.Minute
	}
	for {
		r.discover(ctx, c)
		select {
		case <-ctx.Done():
			return
		case <-time.After(refresh):
		}
	}
}

func (r *Registry) discover(ctx context.Context, c *Client) {
	models, err := discoverModels(ctx, c)
	if err != nil {
		r.log.Warn().Err(err).Str("provider", c.Config.ID).Msg("model discovery failed; using models.yaml only")
		return
	}
	r.rebuildModels(c.Config.ID, models)
	r.log.Info().Str("provider", c.Config.ID).Int("models", len(models)).Msg("models discovered")
}

// rebuildModels merges, per provider, the YAML models over its last discovered
// list: YAML entries win field by field; other discovered models are appended.
// A provider's discovery result is recorded first when one is given.
func (r *Registry) rebuildModels(providerID string, discovered []config.Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if providerID != "" {
		r.discovered[providerID] = discovered
	}
	var out []config.Model
	for _, c := range r.clients {
		out = append(out, merge(c.Config, r.discovered[c.Config.ID])...)
	}
	r.models = out
}

func merge(p config.Provider, discovered []config.Model) []config.Model {
	byID := map[string]config.Model{}
	for _, m := range discovered {
		byID[m.ID] = m
	}
	out := make([]config.Model, 0, len(p.Models)+len(discovered))
	declared := map[string]bool{}
	for _, m := range p.Models {
		declared[m.ID] = true
		if d, ok := byID[m.ID]; ok {
			m = overlay(d, m)
		}
		out = append(out, m)
	}
	for _, m := range discovered {
		if !declared[m.ID] {
			m.ProviderID, m.ProviderName = p.ID, p.Name
			out = append(out, m)
		}
	}
	return out
}

// overlay returns base with every non-zero field of top applied.
func overlay(base, top config.Model) config.Model {
	if top.Name != "" {
		base.Name = top.Name
	}
	if top.Type != "" {
		base.Type = top.Type
	}
	if top.OwnedBy != "" {
		base.OwnedBy = top.OwnedBy
	}
	if top.Description != "" {
		base.Description = top.Description
	}
	if top.ContextLength != 0 {
		base.ContextLength = top.ContextLength
	}
	if top.Capabilities != nil {
		base.Capabilities = top.Capabilities
	}
	if top.SupportedParameters != nil {
		base.SupportedParameters = top.SupportedParameters
	}
	if top.Architecture != nil {
		base.Architecture = top.Architecture
	}
	base.ProviderID, base.ProviderName = top.ProviderID, top.ProviderName
	return base
}
