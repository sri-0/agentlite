// Package mcp turns the mcp connectors of connectors.yaml into adk toolsets.
package mcp

import (
	"context"
	"os/exec"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"agentlite/internal/config"
	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// Server is one configured MCP server: the adk toolset runs use, and the
// transport a short-lived session can be opened on to describe it.
type Server struct {
	Toolset   tool.Toolset
	config    *config.MCP
	transport mcp.Transport
}

// Servers builds one server per mcp connector, keyed by connector id. adk
// connects lazily, so each toolset is wrapped to report a server that cannot
// list its tools as a warning rather than failing every run that would use
// it. A server that cannot be set up is logged and left out.
func Servers(connectors []config.Connector, log zerolog.Logger) map[string]*Server {
	out := map[string]*Server{}
	for _, c := range connectors {
		if c.Type != config.ConnectorMCP {
			continue
		}
		srv, err := newServer(c, log)
		if err != nil {
			log.Error().Err(err).Str("connector", c.ID).Msg("mcp server skipped")
			continue
		}
		out[c.ID] = srv
		log.Info().Str("connector", c.ID).Msg("mcp server configured")
	}
	return out
}

func newServer(c config.Connector, log zerolog.Logger) (*Server, error) {
	t, err := mcpTransport(c)
	if err != nil {
		return nil, err
	}
	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport: t,
		RequireConfirmationProvider: func(toolName string, _ any) bool {
			return slices.Contains(c.MCP.RequireConfirmation, toolName)
		},
	})
	if err != nil {
		return nil, err
	}
	if len(c.MCP.Tools) > 0 {
		ts = tool.FilterToolset(ts, tool.AllowedToolsPredicate(c.MCP.Tools))
	}
	return &Server{Toolset: resilient{Toolset: ts, name: c.ID, log: log}, config: c.MCP, transport: t}, nil
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

// resilient turns a failed tool listing into an empty one, and names the
// toolset after its connector (adk's MCP toolsets are all "mcp_tool_set").
type resilient struct {
	tool.Toolset
	name string
	log  zerolog.Logger
}

func (r resilient) Name() string { return r.name }

func (r resilient) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	tools, err := r.Toolset.Tools(ctx)
	if err != nil {
		r.log.Warn().Err(err).Str("connector", r.name).Msg("mcp tools unavailable")
		return nil, nil
	}
	return tools, nil
}

// mcpTransport reaches the server over streamable HTTP, with a client of its
// own (pkg/transport, from the connector's http block), or over stdio.
func mcpTransport(c config.Connector) (mcp.Transport, error) {
	if c.MCP.Endpoint == "" {
		return commandTransport(c.MCP.Command), nil
	}
	cfg, err := c.HTTP.Transport()
	if err != nil {
		return nil, err
	}
	client, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	return outsideRun{&mcp.StreamableClientTransport{Endpoint: c.MCP.Endpoint, HTTPClient: client}}, nil
}

// outsideRun connects without the thread of the run that happened to need
// the session first. The session's standalone event stream outlives that
// run, so it carries no thread; each request (initialize, tools/list,
// tools/call) still carries the thread of the run that sent it.
type outsideRun struct{ mcp.Transport }

func (t outsideRun) Connect(ctx context.Context) (mcp.Connection, error) {
	return t.Transport.Connect(toolx.WithRun(ctx, toolx.Run{}))
}

// commandTransport starts a fresh process on every Connect, so adk's
// reconnection after a dropped stdio session works; a single exec.Cmd can
// only be started once.
type commandTransport []string

func (argv commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	return (&mcp.CommandTransport{Command: exec.Command(argv[0], argv[1:]...)}).Connect(ctx)
}
