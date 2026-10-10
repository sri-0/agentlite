// Package mcp turns config/default/mcp.yaml into adk toolsets.
package mcp

import (
	"context"
	"net/http"
	"os/exec"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"agentlite/internal/config"
	"agentlite/internal/proxy"
)

// Server is one configured MCP server: the adk toolset runs use, and the
// transport a short-lived session can be opened on to describe it.
type Server struct {
	Toolset   tool.Toolset
	config    config.MCPServer
	transport mcp.Transport
}

// Servers builds one server per entry. adk connects lazily, so each toolset is
// wrapped to report a server that cannot list its tools as a warning rather
// than failing every run that would use it.
func Servers(cfg *config.MCPConfig, log zerolog.Logger) map[string]*Server {
	out := map[string]*Server{}
	for name, s := range cfg.Servers {
		t := transport(s)
		ts, err := mcptoolset.New(mcptoolset.Config{
			Transport: t,
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
		out[name] = &Server{Toolset: resilient{Toolset: ts, server: name, log: log}, config: s, transport: t}
		log.Info().Str("server", name).Msg("mcp server configured")
	}
	return out
}

// URL is how the server is reached: its endpoint, or the command that starts it.
func (s *Server) URL() string {
	if s.config.Endpoint != "" {
		return s.config.Endpoint
	}
	return strings.Join(s.config.Command, " ")
}

// Tools lists the server's tools over a fresh session, honouring the
// configured `tools` filter.
func (s *Server) Tools(ctx context.Context) ([]*mcp.Tool, error) {
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agentlite", Version: "1"}, nil).Connect(ctx, s.transport, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	if len(s.config.Tools) == 0 {
		return res.Tools, nil
	}
	return slices.DeleteFunc(res.Tools, func(t *mcp.Tool) bool { return !slices.Contains(s.config.Tools, t.Name) }), nil
}

// resilient turns a failed tool listing into an empty one.
type resilient struct {
	tool.Toolset
	server string
	log    zerolog.Logger
}

func (r resilient) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := r.Toolset.Tools(ctx)
	if err != nil {
		r.log.Warn().Err(err).Str("server", r.server).Msg("mcp tools unavailable")
		return nil, nil
	}
	return tools, nil
}

func transport(s config.MCPServer) mcp.Transport {
	if s.Endpoint != "" {
		return &mcp.StreamableClientTransport{
			Endpoint:   s.Endpoint,
			HTTPClient: &http.Client{Transport: proxy.WithHeaders(http.DefaultTransport, s.Headers)},
		}
	}
	return commandTransport(s.Command)
}

// commandTransport starts a fresh process on every Connect, so adk's
// reconnection after a dropped stdio session works; a single exec.Cmd can
// only be started once.
type commandTransport []string

func (argv commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return (&mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}).Connect(ctx)
}
