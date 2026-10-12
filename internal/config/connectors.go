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
	for _, c := range cfg.Connectors {
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("connectors.yaml: a connector has no id")
		case seen[c.ID]:
			return nil, fmt.Errorf("connectors.yaml: duplicate connector %q", c.ID)
		case (len(c.Tools) == 0) == (c.MCP == ""):
			return nil, fmt.Errorf("connectors.yaml: connector %q needs tools or an mcp server, not both", c.ID)
		case c.MCP != "" && servers[c.MCP].Endpoint == "" && len(servers[c.MCP].Command) == 0:
			return nil, fmt.Errorf("connectors.yaml: connector %q names unknown mcp server %q", c.ID, c.MCP)
		}
		seen[c.ID] = true
		for k, v := range c.HTTP.Headers {
			c.HTTP.Headers[k] = os.ExpandEnv(v)
		}
	}
	return cfg.Connectors, nil
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
