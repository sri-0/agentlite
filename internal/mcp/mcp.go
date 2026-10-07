// Package mcp turns config/default/mcp.yaml into adk toolsets.
package mcp

import (
	"net/http"
	"os/exec"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"agentlite/internal/config"
	"agentlite/internal/proxy"
)

// Toolsets builds one toolset per configured server. A server that cannot be
// set up is logged and skipped so a run never depends on it.
func Toolsets(cfg *config.MCPConfig, log zerolog.Logger) []tool.Toolset {
	var out []tool.Toolset
	for name, s := range cfg.Servers {
		ts, err := mcptoolset.New(mcptoolset.Config{
			Transport: transport(s),
			RequireConfirmationProvider: func(toolName string, _ any) bool {
				return slices.Contains(s.RequireConfirmation, toolName)
			},
		})
		if err != nil {
			log.Error().Err(err).Str("server", name).Msg("mcp server skipped")
			continue
		}
		if len(s.Tools) > 0 {
			ts = tool.FilterToolset(ts, tool.AllowedToolsPredicate(s.Tools))
		}
		out = append(out, ts)
		log.Info().Str("server", name).Msg("mcp server configured")
	}
	return out
}

func transport(s config.MCPServer) mcp.Transport {
	if s.Endpoint != "" {
		return &mcp.StreamableClientTransport{
			Endpoint:   s.Endpoint,
			HTTPClient: &http.Client{Transport: proxy.WithHeaders(http.DefaultTransport, s.Headers)},
		}
	}
	return &mcp.CommandTransport{Command: exec.Command(s.Command[0], s.Command[1:]...)}
}
