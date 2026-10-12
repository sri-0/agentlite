// Package proxy owns the upstream providers: one HTTP client per provider
// (pkg/transport), model discovery, and the reverse proxies for the
// pass-through routes.
package proxy

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"agentlite/internal/config"
	"agentlite/pkg/transport"
)

// Client is a configured upstream provider. Its HTTP client carries every
// call to the provider: agent model calls, the pass-through proxies and
// embeddings.
type Client struct {
	Config  config.Provider
	BaseURL *url.URL
	APIKey  string
	HTTP    *http.Client
}

// NewClient builds the provider's client from its configuration.
func NewClient(p config.Provider) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("provider %s: invalid base_url %q", p.ID, p.BaseURL)
	}
	tls, err := tlsFiles(p)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	httpClient, err := transport.New(transport.Config{Headers: p.Headers, TLS: tls, DisableCompression: true})
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	return &Client{Config: p, BaseURL: base, APIKey: os.Getenv(p.APIKeyEnv), HTTP: httpClient}, nil
}

// Authorization returns the header value for the provider, or "".
func (c *Client) Authorization() string {
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
}

// tlsFiles reads the provider's TLS settings: models.yaml names environment
// variables that hold the file paths. A variable named but not set is an
// error, never a silent fall back to plain TLS. Nil when none are named.
func tlsFiles(p config.Provider) (*transport.TLS, error) {
	if p.SSLClientCertEnv == "" && p.SSLTrustStoreEnv == "" && !p.SSLInsecureSkipVerify {
		return nil, nil
	}
	for _, env := range []string{p.SSLClientCertEnv, p.SSLClientKeyEnv, p.SSLTrustStoreEnv} {
		if env != "" && os.Getenv(env) == "" {
			return nil, fmt.Errorf("%s is not set", env)
		}
	}
	return &transport.TLS{
		CertFile: os.Getenv(p.SSLClientCertEnv), KeyFile: os.Getenv(p.SSLClientKeyEnv),
		CAFile: os.Getenv(p.SSLTrustStoreEnv), InsecureSkipVerify: p.SSLInsecureSkipVerify,
	}, nil
}
