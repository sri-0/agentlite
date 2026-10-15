package handler

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/labstack/echo/v5"

	"agentlite/internal/config"
	"agentlite/pkg/modelcatalog"
)

// opencodeManifest is what `opencode auth login <server>` reads from
// /.well-known/opencode: how to log in, and configuration that opencode
// fetches on every start and puts under the user's own
// (https://opencode.ai/docs/config).
type opencodeManifest struct {
	Auth   opencodeAuth   `json:"auth"`
	Config opencodeConfig `json:"config"`
}

// opencodeAuth is the command whose output opencode keeps as a token, in the
// variable Env. Clients prove who they are with their certificate (mutual
// TLS), so there is no token: true succeeds and prints none.
type opencodeAuth struct {
	Command []string `json:"command"`
	Env     string   `json:"env"`
}

type opencodeConfig struct {
	Model    string                      `json:"model,omitempty"`
	Provider map[string]opencodeProvider `json:"provider"`
	MCP      map[string]opencodeMCP      `json:"mcp,omitempty"`
	Skills   opencodeSkills              `json:"skills"`
}

// opencodeProvider is the pass-through as an OpenAI-compatible provider.
type opencodeProvider struct {
	NPM     string                   `json:"npm"`
	Name    string                   `json:"name"`
	Options opencodeOptions          `json:"options"`
	Models  map[string]opencodeModel `json:"models"`
}

type opencodeOptions struct {
	BaseURL string `json:"baseURL"`
}

// opencodeModel says what a model can do as far as its card or provider says;
// opencode assumes the rest, tool calls among them.
type opencodeModel struct {
	Name       string        `json:"name,omitempty"`
	ToolCall   *bool         `json:"tool_call,omitempty"`
	Reasoning  bool          `json:"reasoning,omitempty"`
	Attachment bool          `json:"attachment,omitempty"`
	Limit      opencodeLimit `json:"limit,omitzero"`
}

type opencodeLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// opencodeMCP is an exposed MCP connector; the gateway's mutual TLS is the
// authentication, so opencode does not look for OAuth.
type opencodeMCP struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	OAuth bool   `json:"oauth"`
}

type opencodeSkills struct {
	URLs []string `json:"urls"`
}

// Opencode serves /.well-known/opencode: this server's LLMs as a provider
// named after the app, the exposed MCP connectors, and the skills library.
// URLs are this server's as the request reached it, so a gateway in front
// passes Host and X-Forwarded-Proto through.
func (h *Handler) Opencode(c *echo.Context) error {
	base := c.Scheme() + "://" + c.Request().Host
	provider := opencodeProvider{
		NPM: "@ai-sdk/openai-compatible", Name: h.Config.AppName,
		Options: opencodeOptions{BaseURL: base + "/v1"}, Models: map[string]opencodeModel{},
	}
	for _, m := range h.Registry.Models() {
		if m.Type == modelcatalog.TypeLLM {
			provider.Models[m.ID] = opencodeModelOf(m)
		}
	}
	cfg := opencodeConfig{
		Provider: map[string]opencodeProvider{h.Config.AppName: provider},
		MCP:      map[string]opencodeMCP{},
		Skills:   opencodeSkills{URLs: []string{base + "/.well-known/agent-skills/"}},
	}
	// opencode refuses a default its provider does not list, and discovery
	// may not have listed it yet.
	def := h.Config.Models.DefaultModel
	if _, ok := provider.Models[def]; ok {
		cfg.Model = h.Config.AppName + "/" + def
	}
	for _, conn := range h.Config.Connectors {
		if conn.Type == config.ConnectorMCP && conn.MCP.Expose {
			cfg.MCP[conn.ID] = opencodeMCP{Type: "remote", URL: base + "/mcp/" + conn.ID}
		}
	}
	return c.JSON(http.StatusOK, opencodeManifest{
		Auth:   opencodeAuth{Command: []string{"true"}, Env: "AGENTLITE_TOKEN"},
		Config: cfg,
	})
}

func opencodeModelOf(m modelcatalog.Model) opencodeModel {
	out := opencodeModel{
		Name:       m.Name,
		Reasoning:  slices.Contains(m.Capabilities, "reasoning"),
		Attachment: slices.Contains(m.Capabilities, "vision"),
	}
	if len(m.Capabilities) > 0 { // known: a model without tool calls says so
		tools := slices.Contains(m.Capabilities, "tool_calling")
		out.ToolCall = &tools
	}
	if m.ContextLength > 0 && m.MaxOutputTokens > 0 { // opencode needs both
		out.Limit = opencodeLimit{Context: m.ContextLength, Output: m.MaxOutputTokens}
	}
	return out
}

// MCPProxy passes a client's MCP requests to an exposed connector's server.
func (h *Handler) MCPProxy(c *echo.Context) error {
	srv := h.Runtime.ExposedMCP(c.Param("id"))
	if srv == nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no exposed MCP server %q", c.Param("id")))
	}
	if err := srv.Forward(c.Response(), c.Request()); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "the MCP server could not be reached").Wrap(err)
	}
	return nil
}
