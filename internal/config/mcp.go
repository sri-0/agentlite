package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	Headers             map[string]string `yaml:"headers"`
	Tools               []string          `yaml:"tools"`                // omitted ⇒ every tool the server offers
	RequireConfirmation []string          `yaml:"require_confirmation"` // tool names that need approval
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
	for name, s := range cfg.Servers {
		if s.Endpoint == "" && len(s.Command) == 0 {
			return nil, fmt.Errorf("mcp.yaml: server %q needs endpoint or command", name)
		}
		for k, v := range s.Headers {
			s.Headers[k] = os.ExpandEnv(v)
		}
		s.Endpoint = os.ExpandEnv(s.Endpoint)
		cfg.Servers[name] = s
	}
	return &cfg, nil
}
