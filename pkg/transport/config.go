package transport

import (
	"net/http"
	"time"
)

// Env is the transport's configuration from the environment (TRANSPORT_*),
// for providers, MCP servers and tools; the databases have clients of their
// own.
type Env struct {
	// SensitiveHeaders are a caller's headers Forward never passes on: who
	// the caller is. An upstream gets credentials from its own client
	// (Config), or from rewrite when the caller's should go through.
	SensitiveHeaders []string `env:"SENSITIVE_HEADERS, default=Authorization,Cookie,X-Api-Key,X-Forwarded-Client-Cert"`
	// ResponseHeaderTimeout bounds the wait for an upstream's response
	// headers, so a stalled one cannot hold a run; a streamed body runs on.
	ResponseHeaderTimeout time.Duration `env:"RESPONSE_HEADER_TIMEOUT, default=5m"`
}

// Client returns a client for one system (New) with the environment's
// settings.
func (e Env) Client(cfg Config) (*http.Client, error) {
	cfg.ResponseHeaderTimeout = e.ResponseHeaderTimeout
	return New(cfg)
}
