package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/goccy/go-yaml"

	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// ConnectorsConfig is config/default/connectors.yaml: the tool groups a user
// can switch on for a thread. Harness tools are always available.
type ConnectorsConfig struct {
	Connectors []Connector `yaml:"connectors"`
}

// Connector is a set of function tools, or one MCP server from mcp.yaml.
type Connector struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description,omitempty"`
	Icon        string   `yaml:"icon" json:"icon,omitempty"` // a lucide icon name
	Tools       []string `yaml:"tools" json:"tools,omitempty"`
	MCP         string   `yaml:"mcp" json:"mcp,omitempty"`
	// HTTP configures the client the connector's tools reach their system
	// with; each connector gets its own.
	HTTP HTTP `yaml:"http" json:"-"`
}

// HTTP configures one system's HTTP client (pkg/transport).
type HTTP struct {
	Headers map[string]string `yaml:"headers"` // sent on every request; values may use ${ENV}
	Timeout time.Duration     `yaml:"timeout"` // per request; 0 means none
	// ThreadHeaders sends X-Thread-ID, X-Root-Thread-ID and X-Run-ID with
	// each request. Off by default: thread ids are internal, and a tool such
	// as webfetch talks to the public web.
	ThreadHeaders bool `yaml:"thread_headers"`
}

func (h HTTP) configured() bool { return len(h.Headers) > 0 || h.Timeout != 0 || h.ThreadHeaders }

func hasServer(servers map[string]MCPServer, name string) bool {
	_, ok := servers[name]
	return ok
}

// Transport is the client configuration for pkg/transport.
func (h HTTP) Transport() transport.Config {
	cfg := transport.Config{Headers: h.Headers, Timeout: h.Timeout}
	if h.ThreadHeaders {
		cfg.ContextHeaders = toolx.Headers
	}
	return cfg
}

// LoadConnectors reads connectors.yaml; a missing file means no connectors.
func LoadConnectors(dir string, servers map[string]MCPServer) ([]Connector, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "connectors.yaml"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connectors.yaml: %w", err)
	}
	var cfg ConnectorsConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("connectors.yaml: %w", err)
	}
	seen := map[string]bool{}
	listedBy := map[string]string{} // tool → the connector that lists it
	for _, c := range cfg.Connectors {
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("connectors.yaml: a connector has no id")
		case seen[c.ID]:
			return nil, fmt.Errorf("connectors.yaml: duplicate connector %q", c.ID)
		case (len(c.Tools) == 0) == (c.MCP == ""):
			return nil, fmt.Errorf("connectors.yaml: connector %q needs tools or an mcp server, not both", c.ID)
		case c.MCP != "" && !hasServer(servers, c.MCP):
			return nil, fmt.Errorf("connectors.yaml: connector %q names unknown mcp server %q", c.ID, c.MCP)
		case c.MCP != "" && c.HTTP.configured():
			return nil, fmt.Errorf("connectors.yaml: connector %q: set an mcp server's http settings in mcp.yaml", c.ID)
		}
		// A run gets every connector by default, and adk refuses two tools of one name.
		for _, tool := range c.Tools {
			if other, ok := listedBy[tool]; ok {
				return nil, fmt.Errorf("connectors.yaml: tool %q is in connectors %q and %q", tool, other, c.ID)
			}
			listedBy[tool] = c.ID
		}
		seen[c.ID] = true
		expandEnv(c.HTTP.Headers)
	}
	return cfg.Connectors, nil
}

// expandEnv replaces ${VAR} in header values.
func expandEnv(headers map[string]string) {
	for k, v := range headers {
		headers[k] = os.ExpandEnv(v)
	}
}

// CheckConnectors rejects ids that are not configured.
func (c *Config) CheckConnectors(ids []string) error {
	for _, id := range ids {
		if !slices.ContainsFunc(c.Connectors, func(k Connector) bool { return k.ID == id }) {
			return fmt.Errorf("unknown connector %q", id)
		}
	}
	return nil
}
