package agents

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/adk/v2/tool"

	"agentlite/internal/config"
	"agentlite/internal/runs"
	"agentlite/internal/tools"
	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// connectorSets builds one toolset per connector: the MCP server it points
// at, or the world tools it names, built with an HTTP client of the
// connector's own. Tools no connector names are built on a plain client, for
// agents that list them by name.
func (rt *Runtime) connectorSets() error {
	rt.world, rt.sets = map[string]toolx.Tool{}, map[string]tool.Toolset{}
	for _, c := range rt.connectors {
		if c.MCP != "" {
			srv, ok := rt.servers[c.MCP]
			if !ok {
				return fmt.Errorf("connector %q: mcp server %q is not configured", c.ID, c.MCP)
			}
			rt.sets[c.ID] = srv.Toolset
			continue
		}
		built, err := rt.buildTools(c.Tools, c.HTTP.Transport())
		if err != nil {
			return fmt.Errorf("connector %q: %w", c.ID, err)
		}
		rt.sets[c.ID] = toolx.Toolset(c.ID, built)
	}
	var unlisted []string
	for name := range tools.Catalog {
		if _, ok := rt.world[name]; !ok {
			unlisted = append(unlisted, name)
		}
	}
	_, err := rt.buildTools(unlisted, transport.Config{})
	return err
}

// buildTools builds the named world tools on one new client and records them
// in rt.world.
func (rt *Runtime) buildTools(names []string, cfg transport.Config) ([]toolx.Tool, error) {
	client, err := transport.New(cfg)
	if err != nil {
		return nil, err
	}
	built := make([]toolx.Tool, 0, len(names))
	for _, name := range names {
		build, ok := tools.Catalog[name]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		t, err := build(client)
		if err != nil {
			return nil, err
		}
		rt.world[name] = t
		built = append(built, t)
	}
	return built, nil
}

// enabled returns the toolsets of the run's connectors, in connectors.yaml
// order; a nil choice means every connector.
func (rt *Runtime) enabled(choice *[]string) []tool.Toolset {
	var out []tool.Toolset
	for _, c := range rt.connectors {
		if choice == nil || slices.Contains(*choice, c.ID) {
			out = append(out, rt.sets[c.ID])
		}
	}
	return out
}

// Connector types.
const (
	ConnectorTools = "tools" // a set of function tools
	ConnectorMCP   = "mcp"   // one MCP server
)

// ConnectorInfo describes a connector to clients.
type ConnectorInfo struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Icon        string     `json:"icon,omitempty"`
	Type        string     `json:"type"`
	URL         string     `json:"url,omitempty"`    // mcp: endpoint or command
	Tools       []ToolInfo `json:"tools,omitempty"`  // mcp: only from Connector, listed live
	Status      string     `json:"status,omitempty"` // mcp, from Connector: ok | unreachable
	Error       string     `json:"error,omitempty"`
}

// ToolInfo is one tool of a connector.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Connectors describes every connector without touching the network.
func (rt *Runtime) Connectors() []ConnectorInfo {
	out := make([]ConnectorInfo, 0, len(rt.connectors))
	for _, c := range rt.connectors {
		out = append(out, rt.describe(c))
	}
	return out
}

// Connector describes one connector; an MCP connector's tools are listed from
// the server, and a server that cannot be reached is reported, not failed.
func (rt *Runtime) Connector(ctx context.Context, id string) (*ConnectorInfo, error) {
	i := slices.IndexFunc(rt.connectors, func(c config.Connector) bool { return c.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("%w: connector %s", runs.ErrNotFound, id)
	}
	c := rt.connectors[i]
	info := rt.describe(c)
	if c.MCP == "" {
		return &info, nil
	}
	tools, err := rt.servers[c.MCP].Tools(ctx)
	if err != nil {
		info.Status, info.Error = "unreachable", err.Error()
		return &info, nil
	}
	info.Status = "ok"
	for _, t := range tools {
		info.Tools = append(info.Tools, ToolInfo{Name: t.Name, Description: t.Description})
	}
	return &info, nil
}

func (rt *Runtime) describe(c config.Connector) ConnectorInfo {
	info := ConnectorInfo{ID: c.ID, Name: c.Name, Description: c.Description, Icon: c.Icon, Type: ConnectorTools}
	if c.MCP != "" {
		info.Type, info.URL = ConnectorMCP, rt.servers[c.MCP].URL()
		return info
	}
	for _, name := range c.Tools {
		info.Tools = append(info.Tools, ToolInfo{Name: name, Description: rt.world[name].Description()})
	}
	return info
}
