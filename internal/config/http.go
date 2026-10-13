package config

import (
	"fmt"
	"maps"
	"os"
	"time"

	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// HTTP configures the client one system is reached with (pkg/transport): a
// connector's tools or its MCP server.
type HTTP struct {
	Headers   map[string]string `yaml:"headers"`    // sent on every request; values may use ${ENV}
	UserAgent string            `yaml:"user_agent"` // defaults to connectors.yaml's user_agent
	// Timeout bounds each request. Tools only: on an MCP server it would cut
	// the session's long-lived event stream.
	Timeout time.Duration `yaml:"timeout"`
	// Forward sends who a call is for with each request: the user's
	// Authorization header, the thread and run, and for toolx tools the agent
	// and tool. Off by default: a tool such as webfetch talks to the public web.
	Forward bool `yaml:"forward"`
	TLS     *TLS `yaml:"tls"`
}

// Transport is the pkg/transport configuration this block describes.
func (h HTTP) Transport() (transport.Config, error) {
	tls, err := h.TLS.Resolve()
	if err != nil {
		return transport.Config{}, err
	}
	headers := maps.Clone(h.Headers)
	if h.UserAgent != "" {
		if headers == nil {
			headers = map[string]string{}
		}
		headers["User-Agent"] = h.UserAgent
	}
	cfg := transport.Config{Headers: headers, Timeout: h.Timeout, TLS: tls}
	if h.Forward {
		cfg.ContextHeaders = toolx.Headers
	}
	return cfg, nil
}

// TLS names the environment variables that hold a client certificate for
// mutual TLS and the authorities to trust. Each holds a file path, except
// KeyPasswordEnv, which holds the password that unlocks an encrypted key.
type TLS struct {
	CertEnv            string `yaml:"cert_env"`
	KeyEnv             string `yaml:"key_env"`
	KeyPasswordEnv     string `yaml:"key_password_env"`
	CAEnv              string `yaml:"ca_env"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// Resolve reads the variables. One that is named but not set is an error,
// never a silent fall back to plain TLS. Nil stays nil.
func (t *TLS) Resolve() (*transport.TLS, error) {
	if t == nil {
		return nil, nil
	}
	var missing []string
	read := func(env string) string {
		if env == "" {
			return ""
		}
		v := os.Getenv(env)
		if v == "" {
			missing = append(missing, env)
		}
		return v
	}
	out := &transport.TLS{
		CertFile:           read(t.CertEnv),
		KeyFile:            read(t.KeyEnv),
		KeyPassword:        []byte(read(t.KeyPasswordEnv)),
		CAFile:             read(t.CAEnv),
		InsecureSkipVerify: t.InsecureSkipVerify,
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("tls: %v not set", missing)
	}
	if (t.CertEnv == "") != (t.KeyEnv == "") {
		return nil, fmt.Errorf("tls: cert_env and key_env go together")
	}
	return out, nil
}

// expandEnv replaces ${VAR} in header values.
func expandEnv(headers map[string]string) {
	for k, v := range headers {
		headers[k] = os.ExpandEnv(v)
	}
}
