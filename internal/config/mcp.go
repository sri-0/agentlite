package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/goccy/go-yaml"
)

// MCPConfig is config/default/mcp.yaml. Each server becomes one adk toolset;
// its tools keep their MCP names.
type MCPConfig struct {
	Servers map[string]MCPServer `yaml:"servers"`
}

// MCPServer is reached over streamable HTTP (Endpoint) or stdio (Command).
type MCPServer struct {
	Endpoint            string            `yaml:"endpoint"`
	Command             []string          `yaml:"command"`
	Headers             map[string]string `yaml:"headers"`              // values may use ${ENV}
	ThreadHeaders       bool              `yaml:"thread_headers"`       // as for connectors (HTTP)
	Tools               []string          `yaml:"tools"`                // omitted ⇒ every tool the server offers
	RequireConfirmation []string          `yaml:"require_confirmation"` // tool names that need approval

	// ThreadArgs names, per tool, the argument the harness fills with the
	// conversation's root thread id, so the model never has to supply it.
	ThreadArgs map[string]string `yaml:"thread_args"`
	// UI names, per tool, the renderer the UI uses for its calls.
	UI map[string]string `yaml:"ui"`
	// Artifacts records, per tool, its result as a url artifact.
	Artifacts map[string]URLArtifact `yaml:"artifacts"`
}

// HTTP is the server's client configuration. It has no timeout: a client
// timeout also bounds reading a response, which would cut the session's
// long-lived event stream.
func (s MCPServer) HTTP() HTTP {
	return HTTP{Headers: s.Headers, ThreadHeaders: s.ThreadHeaders}
}

// hooked lists the tools the server's hook maps name.
func (s MCPServer) hooked() []string {
	out := slices.Collect(maps.Keys(s.ThreadArgs))
	out = slices.AppendSeq(out, maps.Keys(s.UI))
	return slices.AppendSeq(out, maps.Keys(s.Artifacts))
}

// URLArtifact names the fields of a tool's structured result that hold a
// page's id, title and URL.
type URLArtifact struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	URL   string `yaml:"url"`
}

// LoadMCP reads mcp.yaml; a missing file means no servers.
func LoadMCP(dir string) (*MCPConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return &MCPConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mcp.yaml: %w", err)
	}
	var cfg MCPConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("mcp.yaml: %w", err)
	}
	hookedBy := map[string]string{} // tool → the server whose hooks name it
	for name, s := range cfg.Servers {
		if s.Endpoint == "" && len(s.Command) == 0 {
			return nil, fmt.Errorf("mcp.yaml: server %q needs endpoint or command", name)
		}
		// The harness applies hooks by tool name, so a name must be one server's.
		for _, tool := range s.hooked() {
			if other, ok := hookedBy[tool]; ok && other != name {
				return nil, fmt.Errorf("mcp.yaml: tool %q has hooks in servers %q and %q", tool, other, name)
			}
			hookedBy[tool] = name
		}
		expandEnv(s.Headers)
		s.Endpoint = os.ExpandEnv(s.Endpoint)
		cfg.Servers[name] = s
	}
	return &cfg, nil
}
