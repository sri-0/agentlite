// Package transport builds the HTTP clients agentlite uses to reach other
// systems: one per LLM provider, one per toolset and one per MCP server. Each
// client has its own connection pool, TLS settings and headers, so one
// system's configuration never leaks into another's.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Config says how to reach one system.
type Config struct {
	// Headers are set on every request, replacing any the caller set.
	Headers map[string]string
	// ContextHeaders, when set, adds headers taken from each request's
	// context, such as the thread a tool call belongs to.
	ContextHeaders func(context.Context) http.Header
	// TLS adds a client certificate or trusted authorities; nil keeps the
	// system defaults.
	TLS *TLS
	// Timeout bounds each request, body included. Zero means no limit, which
	// streamed model responses need.
	Timeout time.Duration
	// DisableCompression stops the client asking for gzip. Streamed responses
	// need it: a compressing upstream may hold chunks back.
	DisableCompression bool
}

// TLS names PEM files.
type TLS struct {
	CertFile, KeyFile  string // client certificate, for mutual TLS
	CAFile             string // trusted certificate authorities
	InsecureSkipVerify bool
}

// New returns a client with its own connection pool.
func New(cfg Config) (*http.Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = true
	t.MaxIdleConnsPerHost = 100
	t.DisableCompression = cfg.DisableCompression
	if cfg.TLS != nil {
		tlsCfg, err := cfg.TLS.config()
		if err != nil {
			return nil, err
		}
		t.TLSClientConfig = tlsCfg
	}
	var rt http.RoundTripper = t
	if len(cfg.Headers) > 0 || cfg.ContextHeaders != nil {
		rt = &headers{base: t, static: cfg.Headers, fromContext: cfg.ContextHeaders}
	}
	return &http.Client{Transport: rt, Timeout: cfg.Timeout}, nil
}

func (c *TLS) config() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipVerify}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("trusted authorities: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("trusted authorities: no certificates in %s", c.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// headers sets the configured headers on a copy of each request; a
// RoundTripper must not change the caller's request.
type headers struct {
	base        http.RoundTripper
	static      map[string]string
	fromContext func(context.Context) http.Header
}

func (h *headers) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.static {
		r.Header.Set(k, v)
	}
	if h.fromContext != nil {
		for k, vs := range h.fromContext(r.Context()) {
			r.Header[http.CanonicalHeaderKey(k)] = vs
		}
	}
	return h.base.RoundTrip(r)
}
