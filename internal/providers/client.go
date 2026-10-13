// Package providers owns the upstream LLM providers: an HTTP client per
// provider (pkg/transport), the models each serves (pkg/modelcatalog), how a
// model id resolves to its provider, the pass-through proxy and embeddings.
package providers

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/internal/config"
	"agentlite/pkg/transport"
)

// Client is a configured upstream provider. Its HTTP client carries every
// call to the provider: agent model calls, the pass-through proxy and
// embeddings.
type Client struct {
	Config  config.Provider
	BaseURL *url.URL
	APIKey  string
	HTTP    *http.Client
	openai  openai.Client // for embeddings
}

// NewClient builds the provider's client from its configuration.
func NewClient(p config.Provider) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("provider %s: invalid base_url %q", p.ID, p.BaseURL)
	}
	tls, err := p.TLS.Resolve()
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	httpClient, err := transport.New(transport.Config{Headers: p.Headers, TLS: tls, DisableCompression: true})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	c := &Client{Config: p, BaseURL: base, APIKey: os.Getenv(p.APIKeyEnv), HTTP: httpClient}
	c.openai = openai.NewClient(option.WithBaseURL(base.String()), option.WithAPIKey(c.APIKey), option.WithHTTPClient(httpClient))
	return c, nil
}

// Authorization returns the header value for the provider, or "".
func (c *Client) Authorization() string {
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
}
