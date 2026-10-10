package engine

import (
	"context"
	"fmt"
	"slices"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"agentlite/internal/agent"
	"agentlite/internal/config"
)

// toolset is a connector as adk sees it: a named set of tools handed to the
// agent as one unit, so switching a connector off removes all of them.
type toolset struct {
	name  string
	tools []tool.Tool
}

func (t toolset) Name() string                                        { return t.name }
func (t toolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) { return t.tools, nil }

// connectorSets builds one toolset per connector: the world tools it names,
// or the MCP server it points at.
func (e *Engine) connectorSets() (map[string]tool.Toolset, error) {
	sets := make(map[string]tool.Toolset, len(e.connectors))
	for _, c := range e.connectors {
		if c.MCP != "" {
			srv, ok := e.servers[c.MCP]
			if !ok {
				return nil, fmt.Errorf("connector %q: mcp server %q is not configured", c.ID, c.MCP)
			}
			sets[c.ID] = srv.Toolset
			continue
		}
		ts := toolset{name: c.ID}
		for _, name := range c.Tools {
			t, ok := e.world[name]
			if !ok {
				return nil, fmt.Errorf("connector %q: unknown tool %q", c.ID, name)
			}
			ts.tools = append(ts.tools, t)
		}
		sets[c.ID] = ts
	}
	return sets, nil
}

// enabled returns the toolsets of the run's connectors, in connectors.yaml
// order; a nil choice means every connector.
func (e *Engine) enabled(choice *[]string) []tool.Toolset {
	var out []tool.Toolset
	for _, c := range e.connectors {
		if choice == nil || slices.Contains(*choice, c.ID) {
			out = append(out, e.sets[c.ID])
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
func (e *Engine) Connectors() []ConnectorInfo {
	out := make([]ConnectorInfo, 0, len(e.connectors))
	for _, c := range e.connectors {
		out = append(out, e.describe(c))
	}
	return out
}

// Connector describes one connector; an MCP connector's tools are listed from
// the server, and a server that cannot be reached is reported, not failed.
func (e *Engine) Connector(ctx context.Context, id string) (*ConnectorInfo, error) {
	i := slices.IndexFunc(e.connectors, func(c config.Connector) bool { return c.ID == id })
	if i < 0 {
		return nil, fmt.Errorf("%w: connector %s", agent.ErrNotFound, id)
	}
	c := e.connectors[i]
	info := e.describe(c)
	if c.MCP == "" {
		return &info, nil
	}
	tools, err := e.servers[c.MCP].Tools(ctx)
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

func (e *Engine) describe(c config.Connector) ConnectorInfo {
	info := ConnectorInfo{ID: c.ID, Name: c.Name, Description: c.Description, Icon: c.Icon, Type: ConnectorTools}
	if c.MCP != "" {
		info.Type, info.URL = ConnectorMCP, e.servers[c.MCP].URL()
		return info
	}
	for _, name := range c.Tools {
		info.Tools = append(info.Tools, ToolInfo{Name: name, Description: e.world[name].Description()})
	}
	return info
}
