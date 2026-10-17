package wellknown

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/skills"
)

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
