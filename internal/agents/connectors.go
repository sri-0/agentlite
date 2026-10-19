package agents

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/adk/v2/tool"

	"agentlite/config"
	"agentlite/internal/mcp"
	"agentlite/internal/runs"
	"agentlite/internal/tools"
	"agentlite/pkg/toolx"
)

// connectorSets builds one toolset per connector: its MCP server, or its
// toolx tools built on one HTTP client of the connector's own. A tool is only
// offered through a connector.
func (rt *Runtime) connectorSets() error {
	rt.world, rt.sets = map[string]toolx.Tool{}, map[string]tool.Toolset{}
	for _, c := range rt.connectors {
		if c.Type == config.ConnectorMCP {
			rt.sets[c.ID] = rt.servers[c.ID].Toolset
			continue
		}
		built, err := rt.buildTools(c)
		if err != nil {
			return fmt.Errorf("connector %q: %w", c.ID, err)
		}
		rt.sets[c.ID] = toolx.Toolset(c.ID, built)
	}
	return nil
}

// buildTools builds a connector's tools on one new client, and records them
// in rt.world.
func (rt *Runtime) buildTools(c config.Connector) ([]toolx.Tool, error) {
	client, err := c.HTTP.Client(rt.transport)
	if err != nil {
		return nil, err
	}
	conn := toolx.Connector{ID: c.ID, HTTP: client}
	built := make([]toolx.Tool, 0, len(c.Tools))
	for _, n := range c.Tools {
		build, ok := tools.Catalog[n]
		if !ok {
			return nil, fmt.Errorf("unknown tool %q", n)
		}
		t, err := build(conn)
		if err != nil {
			return nil, err
		}
		rt.world[n] = t
		built = append(built, t)
	}
	return built, nil
}

// ownSets adds the server's own connectors (tools.Own) after connectors.yaml's:
// image generation when an image model is configured, file search when the
// uploaded files are searchable. Agents that list their tool by name get it too.
func (rt *Runtime) ownSets() error {
	var own []toolx.Tool
	if rt.images != nil {
		t, err := tools.GenerateImage(rt.images)
		if err != nil {
			return err
		}
		own = append(own, t)
	}
	if rt.files.Searchable() {
		t, err := tools.SearchFiles(rt.files)
		if err != nil {
			return err
		}
		own = append(own, t)
	}
	for _, t := range own {
		c := tools.Own[t.Name()]
		if slices.ContainsFunc(rt.connectors, func(k config.Connector) bool { return k.ID == c.ID }) {
			return fmt.Errorf("connector id %q is the server's own; connectors.yaml can't use it", c.ID)
		}
		rt.world[t.Name()] = t
		rt.sets[c.ID] = toolx.Toolset(c.ID, []toolx.Tool{t})
		rt.connectors = append(slices.Clip(rt.connectors), c) // never into the config's array
	}
	return nil
}

// CheckConnectors refuses connector ids the runtime doesn't serve.
func (rt *Runtime) CheckConnectors(ids []string) error {
	for _, id := range ids {
		if _, ok := rt.sets[id]; !ok {
			return fmt.Errorf("unknown connector %q", id)
		}
	}
	return nil
}

// enabled returns the toolsets of the run's connectors, in connectors.yaml
// order then the server's own; a nil choice means every connector.
func (rt *Runtime) enabled(choice *[]string) []tool.Toolset {
	var out []tool.Toolset
	for _, c := range rt.connectors {
		if choice == nil || slices.Contains(*choice, c.ID) {
			out = append(out, rt.sets[c.ID])
		}
	}
	return out
}

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
	if c.Type != config.ConnectorMCP {
		return &info, nil
	}
	tools, err := rt.servers[c.ID].Tools(ctx)
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
	info := ConnectorInfo{ID: c.ID, Name: c.Name, Description: c.Description, Icon: c.Icon, Type: c.Type}
	if c.Type == config.ConnectorMCP {
		info.URL = rt.servers[c.ID].URL()
		return info
	}
	for _, name := range c.Tools {
		info.Tools = append(info.Tools, ToolInfo{Name: name, Description: rt.world[name].Description()})
	}
	return info
}

// ExposedMCP is the MCP server a connector exposes to clients (mcp.expose),
// nil when there is none.
func (rt *Runtime) ExposedMCP(id string) *mcp.Server {
	if srv := rt.servers[id]; srv != nil && srv.Exposed() {
		return srv
	}
	return nil
}
