package wellknown

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/config"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/opencode"
)

// Opencode serves /.well-known/opencode: this server's LLMs as a provider
// named after the app, the MCP connectors it exposes, and the skills library.
// Clients prove who they are with their certificate (mutual TLS), so there is
// no token: the login command, true, succeeds and prints none.
func (h *Handler) Opencode(c *echo.Context) error {
	base, app := h.Config.AppURL, h.Config.AppName
	provider := opencode.Provider{
		NPM: "@ai-sdk/openai-compatible", Name: app,
		Options: opencode.Options{BaseURL: base + "/v1"}, Models: map[string]opencode.Model{},
	}
	for _, m := range h.Registry.Models() {
		if m.Type == modelcatalog.TypeLLM {
			provider.Models[m.ID] = opencode.ModelOf(m)
		}
	}
	cfg := opencode.Config{
		Model:    app + "/" + h.Config.Models.DefaultModel, // checked at the start to be listed
		Provider: map[string]opencode.Provider{app: provider}, MCP: map[string]opencode.MCP{},
	}
	for _, conn := range h.Config.Connectors {
		if conn.Type == config.ConnectorMCP && conn.MCP.Expose {
			cfg.MCP[conn.ID] = opencode.MCP{Type: "remote", URL: base + "/mcp/" + conn.ID}
		}
	}
	cfg.Skills.URLs = []string{base + "/.well-known/agent-skills/"}
	return c.JSON(http.StatusOK, opencode.Manifest{
		Auth:   opencode.Auth{Command: []string{"true"}, Env: "AGENTLITE_TOKEN"},
		Config: cfg,
	})
}
