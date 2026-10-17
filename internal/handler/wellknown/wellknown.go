// Package wellknown serves /.well-known/: what opencode and npx skills read to
// set themselves up from this server.
package wellknown

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/config"
	"agentlite/internal/providers"
	"agentlite/internal/skills"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/opencode"
)

// Handler serves the well-known documents.
type Handler struct {
	Config   *config.Config
	Registry *providers.Registry
	Skills   *skills.Library // nil without S3
}

// Opencode serves /.well-known/opencode: this server's LLMs as a provider
// named after the app, the MCP connectors it exposes, and the skills library.
// Clients prove who they are with their certificate (mutual TLS), so there is
// no token: the login command, true, succeeds and prints none.
func (h *Handler) Opencode(c *echo.Context) error {
	base, app := h.Config.PublicURL, h.Config.AppName
	provider := opencode.Provider{
		NPM: "@ai-sdk/openai-compatible", Name: app,
		Options: opencode.Options{BaseURL: base + "/v1"}, Models: map[string]opencode.Model{},
	}
	for _, m := range h.Registry.Models() {
		if m.Type == modelcatalog.TypeLLM {
			provider.Models[m.ID] = opencode.ModelOf(m)
		}
	}
	cfg := opencode.Config{Provider: map[string]opencode.Provider{app: provider}, MCP: map[string]opencode.MCP{}}
	// models.yaml's default, which opencode refuses unless its provider lists
	// it: discovery may not have listed it yet.
	if def := h.Config.Models.DefaultModel; hasModel(provider, def) {
		cfg.Model = app + "/" + def
	}
	for _, conn := range h.Config.Connectors {
		if conn.Type == config.ConnectorMCP && conn.MCP.Expose {
			cfg.MCP[conn.ID] = opencode.MCP{Type: "remote", URL: base + "/mcp/" + conn.ID}
		}
	}
	if h.Skills != nil {
		cfg.Skills.URLs = []string{base + "/.well-known/agent-skills/"}
	}
	return c.JSON(http.StatusOK, opencode.Manifest{
		Auth:   opencode.Auth{Command: []string{"true"}, Env: "AGENTLITE_TOKEN"},
		Config: cfg,
	})
}

func hasModel(p opencode.Provider, id string) bool {
	_, ok := p.Models[id]
	return ok
}

// SkillsIndex serves /.well-known/agent-skills/index.json, the skills library
// as npx skills and opencode discover it; they send no credentials.
func (h *Handler) SkillsIndex(c *echo.Context) error {
	index, err := h.Skills.Index(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, index)
}

// SkillFile serves /.well-known/agent-skills/<name>/<path>, a file of a skill
// the index names: the one place a skill is addressed by its name.
func (h *Handler) SkillFile(c *echo.Context) error {
	ctx := c.Request().Context()
	s, err := h.Skills.Named(ctx, c.Param("name"))
	if err != nil {
		return err
	}
	p := c.Param("*")
	b, err := h.Skills.File(ctx, s, p)
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, skills.ContentType(p, b), b)
}
