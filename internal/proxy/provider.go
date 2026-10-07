// Package proxy owns the upstream providers: one dedicated HTTP client per
// provider (mTLS and static headers over a shared base transport), model
// discovery, and the reverse proxies for the pass-through routes.
package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"agentlite/internal/config"
)

// Client is a configured upstream provider.
type Client struct {
	Config  config.Provider
	BaseURL *url.URL
	APIKey  string
	HTTP    *http.Client
}

// NewClient builds the provider's dedicated client from its configuration.
func NewClient(p config.Provider) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(p.BaseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("provider %s: invalid base_url %q", p.ID, p.BaseURL)
	}
	transport, err := newTransport(p)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", p.ID, err)
	}
	return &Client{
		Config:  p,
		BaseURL: base,
		APIKey:  os.Getenv(p.APIKeyEnv),
		HTTP:    &http.Client{Transport: transport},
	}, nil
}

// Authorization returns the header value for the provider, or "".
func (c *Client) Authorization() string {
	if c.APIKey == "" {
		return ""
	}
	return "Bearer " + c.APIKey
}

func newTransport(p config.Provider) (http.RoundTripper, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	t.DisableCompression = true // streams must not be buffered by gzip
	t.ResponseHeaderTimeout = 60 * time.Second
	t.MaxIdleConnsPerHost = 100
	tlsCfg, err := tlsConfig(p)
	if err != nil {
		return nil, err
	}
	if tlsCfg != nil {
		t.TLSClientConfig = tlsCfg
	}
	return WithHeaders(t, p.Headers), nil
}

// tlsConfig builds the mTLS configuration when the provider names cert files
// through environment variables (the variables hold file paths).
func tlsConfig(p config.Provider) (*tls.Config, error) {
	if p.SSLClientCertEnv == "" && p.SSLTrustStoreEnv == "" && !p.SSLInsecureSkipVerify {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: p.SSLInsecureSkipVerify}
	if p.SSLClientCertEnv != "" {
		cert, err := tls.LoadX509KeyPair(os.Getenv(p.SSLClientCertEnv), os.Getenv(p.SSLClientKeyEnv))
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if p.SSLTrustStoreEnv != "" {
		pem, err := os.ReadFile(os.Getenv(p.SSLTrustStoreEnv))
		if err != nil {
			return nil, fmt.Errorf("trust store: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("trust store: no certificates in %s", os.Getenv(p.SSLTrustStoreEnv))
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// WithHeaders wraps a transport so every request carries the static headers.
func WithHeaders(base http.RoundTripper, headers map[string]string) http.RoundTripper {
	if len(headers) == 0 {
		return base
	}
	return &headerTransport{base: base, headers: headers}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}
