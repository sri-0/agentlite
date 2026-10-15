package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"agentlite/internal/config"
	"agentlite/pkg/modelcatalog"
)

// Errors Resolve returns. Neither falls back to another provider: a request
// goes to the provider its model id names, or nowhere.
var (
	ErrUnknownProvider = errors.New("unknown provider")
	ErrUnknownModel    = errors.New("unknown model")
)

const (
	discoverTimeout = 30 * time.Second
	discoverBody    = 8 << 20
	defaultRefresh  = 10 * time.Minute
)

// Registry holds every provider and the models each serves.
type Registry struct {
	providers []*provider // in models.yaml order
	cardsDir  string      // MODEL_CARDS_DIR; "" means no cards
	log       zerolog.Logger

	mu    sync.RWMutex // guards each provider's discovered and models, and cards
	cards modelcatalog.Cards
}

type provider struct {
	client   *Client
	declared []modelcatalog.Model // models.yaml, by upstream id
	baseURLs map[string]*url.URL  // the declared models' own base_url, by upstream id

	discovered []modelcatalog.Model          // the provider's last successful listing
	models     map[string]modelcatalog.Model // merged, by upstream id
	order      []string                      // upstream ids: declared first, then discovered
}

// Target is where one model's requests go.
type Target struct {
	Client  *Client
	Model   modelcatalog.Model
	BaseURL *url.URL // the model's base_url, or the provider's
	API     string   // the model's api, or the provider's
}

// NewRegistry builds a client per provider and the model lists from
// models.yaml and the model cards; Start adds what discovery finds.
func NewRegistry(cfg *config.ModelsConfig, cardsDir string, log zerolog.Logger) (*Registry, error) {
	r := &Registry{cardsDir: cardsDir, log: log}
	for _, p := range cfg.Providers {
		if p.BaseURL == "" {
			log.Warn().Str("provider", p.ID).Msg("provider has no base_url in this environment; skipped")
			continue
		}
		c, err := newClient(p)
		if err != nil {
			return nil, err
		}
		prov := &provider{client: c, declared: p.Models, baseURLs: map[string]*url.URL{}}
		for _, m := range p.Models {
			if m.BaseURL == "" {
				continue
			}
			if prov.baseURLs[m.ID], err = parseBaseURL(m.BaseURL); err != nil {
				return nil, fmt.Errorf("provider %s: model %s: %w", p.ID, m.ID, err)
			}
		}
		r.providers = append(r.providers, prov)
	}
	r.loadCards()
	for _, p := range r.providers {
		r.rebuild(p, nil)
	}
	return r, nil
}

// Resolve finds a model by its id, "<provider>/<upstream id>", in that
// provider's own list and nowhere else.
func (r *Registry) Resolve(id string) (Target, error) {
	pid, upstream, _ := strings.Cut(id, "/")
	p := r.provider(pid)
	if p == nil {
		return Target{}, fmt.Errorf("%w %q (model %q)", ErrUnknownProvider, pid, id)
	}
	r.mu.RLock()
	m, ok := p.models[upstream]
	r.mu.RUnlock()
	if !ok {
		return Target{}, fmt.Errorf("%w %q for provider %q", ErrUnknownModel, upstream, pid)
	}
	t := Target{Client: p.client, Model: m, BaseURL: p.client.BaseURL, API: p.client.Config.API}
	if u, ok := p.baseURLs[upstream]; ok {
		t.BaseURL = u
	}
	if m.API != "" {
		t.API = m.API
	}
	return t, nil
}

// Models returns every model of every provider, in models.yaml order.
func (r *Registry) Models() []modelcatalog.Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []modelcatalog.Model{}
	for _, p := range r.providers {
		for _, id := range p.order {
			out = append(out, p.models[id])
		}
	}
	return out
}

func (r *Registry) provider(id string) *provider {
	for _, p := range r.providers {
		if p.client.Config.ID == id {
			return p
		}
	}
	return nil
}

// Start runs discovery for every provider that configures it, once now and
// then on its refresh interval, until ctx ends. Each pass reloads the cards.
func (r *Registry) Start(ctx context.Context) {
	for _, p := range r.providers {
		if p.client.Config.Discover != nil {
			go r.discoverLoop(ctx, p)
		}
	}
}

func (r *Registry) discoverLoop(ctx context.Context, p *provider) {
	refresh := p.client.Config.Discover.Refresh
	if refresh <= 0 {
		refresh = defaultRefresh
	}
	for {
		models, err := discover(ctx, p.client)
		if err != nil {
			r.log.Warn().Err(err).Str("provider", p.client.Config.ID).Msg("model discovery failed; keeping the last listing")
		} else {
			r.loadCards()
			r.rebuild(p, models)
			r.log.Info().Str("provider", p.client.Config.ID).Int("models", len(models)).Msg("models discovered")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(refresh):
		}
	}
}

// discover fetches the provider's model list.
func discover(ctx context.Context, c *Client) ([]modelcatalog.Model, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	endpoint := c.Config.Discover.Endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL.String()+endpoint, nil)
	if err != nil {
		return nil, err
	}
	if auth := c.Authorization(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoverBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	return modelcatalog.Parse(endpoint, body)
}

// loadCards rereads the model cards, so a pulled card needs no restart.
func (r *Registry) loadCards() {
	if r.cardsDir == "" {
		return
	}
	cards, bad := modelcatalog.LoadCards(r.cardsDir)
	if bad != nil {
		r.log.Warn().Err(bad).Msg("some model cards were skipped")
	}
	r.mu.Lock()
	r.cards = cards
	r.mu.Unlock()
}

// rebuild records a discovery listing (nil before the first) and merges, per
// model, its card, what discovery reported and models.yaml, in that order of
// precedence.
func (r *Registry) rebuild(p *provider, discovered []modelcatalog.Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if discovered != nil {
		p.discovered = discovered
	}
	cfg := p.client.Config
	declared := map[string]modelcatalog.Model{}
	var order []string
	for _, m := range p.declared {
		declared[m.ID] = m
		order = append(order, m.ID)
	}
	listed := map[string]modelcatalog.Model{}
	for _, m := range p.discovered {
		listed[m.ID] = m
		if _, ok := declared[m.ID]; !ok {
			order = append(order, m.ID)
		}
	}
	models := make(map[string]modelcatalog.Model, len(order))
	for _, id := range order {
		card, _ := r.cards.For(id)
		m := modelcatalog.Merge(card, listed[id], declared[id])
		m.ID, m.UpstreamID, m.ProviderID, m.ProviderName = cfg.ID+"/"+id, id, cfg.ID, cfg.Name
		if m.Type == "" {
			m.Type = modelcatalog.TypeLLM
		}
		models[id] = m
	}
	p.models, p.order = models, order
}
