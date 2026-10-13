package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// ConnectorsConfig is config/default/connectors.yaml: the systems a user can
// switch on for a thread. Harness tools are always available.
type ConnectorsConfig struct {
	// UserAgent is sent by every connector whose http block sets none.
	UserAgent  string      `yaml:"user_agent"`
	Connectors []Connector `yaml:"connectors"`
}

// Connector types.
const (
	ConnectorTools = "tools" // a set of toolx tools
	ConnectorMCP   = "mcp"   // one MCP server
)

// Connector is one system: a set of toolx tools, or one MCP server. Either
// way the agent gets it as one adk toolset.
type Connector struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Icon        string   `yaml:"icon"` // a lucide icon name
	Type        string   `yaml:"type"` // tools (the default) or mcp
	Tools       []string `yaml:"tools"`
	MCP         *MCP     `yaml:"mcp"`
	HTTP        HTTP     `yaml:"http"`
}

// MCP is an MCP server, reached over streamable HTTP (Endpoint, with the
// connector's http block) or stdio (Command).
type MCP struct {
	Endpoint            string   `yaml:"endpoint"`
	Command             []string `yaml:"command"`
	Tools               []string `yaml:"tools"`                // omitted ⇒ every tool the server offers
	RequireConfirmation []string `yaml:"require_confirmation"` // tool names that need approval

	// ThreadArgs names, per tool, the argument the harness fills with the
	// conversation's root thread id, so the model never has to supply it.
	ThreadArgs map[string]string `yaml:"thread_args"`
	// UI names, per tool, the renderer the UI uses for its calls.
	UI map[string]string `yaml:"ui"`
	// Artifacts records, per tool, its result as a url artifact.
	Artifacts map[string]URLArtifact `yaml:"artifacts"`
}

// URLArtifact names the fields of a tool's structured result that hold a
// page's id, title and URL.
type URLArtifact struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	URL   string `yaml:"url"`
}

// hooked lists the tools the server's hook maps name.
func (m *MCP) hooked() []string {
	out := slices.Collect(maps.Keys(m.ThreadArgs))
	out = slices.AppendSeq(out, maps.Keys(m.UI))
	return slices.AppendSeq(out, maps.Keys(m.Artifacts))
}

const defaultUserAgent = "agentlite/1.0"

// LoadConnectors reads connectors.yaml; a missing file means no connectors.
func LoadConnectors(dir string) (ConnectorsConfig, error) {
	cfg := ConnectorsConfig{UserAgent: defaultUserAgent}
	raw, err := os.ReadFile(filepath.Join(dir, "connectors.yaml"))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("connectors.yaml: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("connectors.yaml: %w", err)
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	seen := map[string]bool{}
	claimed := map[string]string{} // tool → the connector that lists it or hooks it
	for i := range cfg.Connectors {
		c := &cfg.Connectors[i]
		if c.Type == "" {
			c.Type = ConnectorTools
		}
		if c.HTTP.UserAgent == "" {
			c.HTTP.UserAgent = cfg.UserAgent
		}
		expandEnv(c.HTTP.Headers)
		if err := c.check(seen, claimed); err != nil {
			return cfg, fmt.Errorf("connectors.yaml: connector %q: %w", c.ID, err)
		}
	}
	return cfg, nil
}

// check validates one connector. A tool name may belong to one connector
// only: a run gets every connector by default, adk refuses two tools of one
// name, and the MCP hooks apply by tool name.
func (c *Connector) check(seen map[string]bool, claimed map[string]string) error {
	switch {
	case c.ID == "":
		return errors.New("no id")
	case seen[c.ID]:
		return errors.New("duplicate id")
	case c.HTTP.Forward && hasAuthorization(c.HTTP.Headers):
		// Calls with no user (a turn started on the socket, a resumed run)
		// would go out as the static identity: one thread, two identities.
		return errors.New("http.forward sends the user's Authorization; drop the static Authorization header")
	}
	seen[c.ID] = true
	var names []string
	switch c.Type {
	case ConnectorTools:
		if len(c.Tools) == 0 || c.MCP != nil {
			return errors.New("a tools connector lists tools and has no mcp block")
		}
		names = c.Tools
	case ConnectorMCP:
		if c.MCP == nil || len(c.Tools) > 0 {
			return errors.New("an mcp connector has an mcp block and no tools list (filter with mcp.tools)")
		}
		endpoint := os.ExpandEnv(c.MCP.Endpoint)
		switch {
		case c.MCP.Endpoint != "" && endpoint == "":
			return fmt.Errorf("mcp endpoint %s is empty in this environment", c.MCP.Endpoint)
		case (endpoint == "") == (len(c.MCP.Command) == 0):
			return errors.New("mcp needs an endpoint or a command")
		case c.HTTP.Timeout != 0:
			return errors.New("an mcp connector takes no timeout: it would cut the session's event stream")
		}
		c.MCP.Endpoint = endpoint
		names = c.MCP.hooked()
	default:
		return fmt.Errorf("type must be %s or %s", ConnectorTools, ConnectorMCP)
	}
	for _, tool := range names {
		if other, ok := claimed[tool]; ok && other != c.ID {
			return fmt.Errorf("tool %q is also in connector %q", tool, other)
		}
		claimed[tool] = c.ID
	}
	return nil
}

// hasAuthorization reports whether headers set Authorization, in any case.
func hasAuthorization(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
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
