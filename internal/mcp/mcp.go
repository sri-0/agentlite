// Package mcp turns the mcp connectors of connectors.yaml into adk toolsets,
// and proxies the servers they expose to clients such as opencode.
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

	// An exposed server's endpoint, nil otherwise, and how it is reached.
	endpoint *url.URL
	client   *http.Client
	forward  bool          // the connector forwards the user's Authorization
	proxy    transport.Env // how a client's requests are forwarded
}

// Servers builds one server per mcp connector, keyed by connector id. adk
// connects lazily, so each toolset is wrapped to report a server that cannot
// list its tools as a warning rather than failing every run that would use
// it. Setting one up touches no network, so an error is the configuration's
// (a TLS file, say) and fails the start.
func Servers(connectors []config.Connector, proxy transport.Env, log zerolog.Logger) (map[string]*Server, error) {
	out := map[string]*Server{}
	for _, c := range connectors {
		if c.Type != config.ConnectorMCP {
			continue
		}
		srv, err := newServer(c, proxy, log)
		if err != nil {
			return nil, fmt.Errorf("connector %q: %w", c.ID, err)
		}
		out[c.ID] = srv
	}
	return out, nil
}

func newServer(c config.Connector, proxy transport.Env, log zerolog.Logger) (*Server, error) {
	s := &Server{config: c.MCP, transport: commandTransport(c.MCP.Command), forward: c.HTTP.Forward, proxy: proxy}
	if c.MCP.Endpoint != "" {
		client, err := httpClient(c)
		if err != nil {
			return nil, err
		}
		s.client = client
		s.transport = outsideRun{&mcp.StreamableClientTransport{Endpoint: c.MCP.Endpoint, HTTPClient: client}}
		if c.MCP.Expose {
			if s.endpoint, err = url.Parse(c.MCP.Endpoint); err != nil {
				return nil, err
			}
		}
	}
	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport: s.transport,
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
	s.Toolset = resilient{Toolset: ts, name: c.ID, log: log}
	return s, nil
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

// httpClient is the client a server over streamable HTTP is reached with, its
// own (pkg/transport, from the connector's http block).
func httpClient(c config.Connector) (*http.Client, error) {
	cfg, err := c.HTTP.Transport()
	if err != nil {
		return nil, err
	}
	return transport.New(cfg)
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
