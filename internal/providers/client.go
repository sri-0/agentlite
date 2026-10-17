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

	"agentlite/config"
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
}

// newClient builds the provider's client from its configuration.
func newClient(p config.Provider, env transport.Env) (*Client, error) {
	base, err := parseBaseURL(p.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	tls, err := p.TLS.Resolve()
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	httpClient, err := env.Client(transport.Config{Headers: p.Headers, TLS: tls, DisableCompression: true})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	return &Client{Config: p, BaseURL: base, APIKey: os.Getenv(p.APIKeyEnv), HTTP: httpClient}, nil
}

// parseBaseURL reads an absolute http(s) base URL, without a trailing slash.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid base_url %q", raw)
	}
	return u, nil
}

// Authorization returns the header value for the provider, or "".
func (c *Client) Authorization() string {
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
}
