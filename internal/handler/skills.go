package handler

import (
	"io"
	"mime"
	"net/http"
	"path"

	"github.com/labstack/echo/v5"

	"agentlite/internal/skills"
)

// ListSkills lists the skills library by name, each skill with the paths of
// its files.
func (h *Handler) ListSkills(c *echo.Context) error {
	list, err := h.Skills.Skills(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(list))
}

// CreateSkill adds a skill to the library, replacing any of the same name.
// The body is a zip of the skill, or its SKILL.md alone.
func (h *Handler) CreateSkill(c *echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err // the body limit's 413 among others
	}
	s, err := skills.Read(body)
	if err != nil {
		return err
	}
	if err := h.Skills.SaveSkill(c.Request().Context(), s); err != nil {
		return err
	}
	for i := range s.Files {
		s.Files[i].Content = nil
	}
	return c.JSON(http.StatusCreated, s)
}

// DeleteSkill removes a skill from the library.
func (h *Handler) DeleteSkill(c *echo.Context) error {
	if err := h.Skills.DeleteSkill(c.Request().Context(), c.Param("name")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// SkillFile serves one of a skill's files as written; like an artifact's
// body, it is sandboxed and never sniffed.
func (h *Handler) SkillFile(c *echo.Context) error {
	p := c.Param("*")
	b, err := h.Skills.File(c.Request().Context(), c.Param("name"), p)
	if err != nil {
		return err
	}
	ctype := mime.TypeByExtension(path.Ext(p))
	if ctype == "" {
		ctype = http.DetectContentType(b)
	}
	c.Response().Header().Set("Content-Security-Policy", "sandbox")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, ctype, b)
}

// skillsIndex is the library as npx skills discovers it (v0.1.0 of the
// discovery draft, github.com/cloudflare/agent-skills-discovery-rfc): each
// skill with the paths of its files, which it fetches from <name>/<path>
// next to the index.
type skillsIndex struct {
	Skills []skillsIndexEntry `json:"skills"`
}

type skillsIndexEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

// SkillsIndex serves the discovery index; npx skills sends no credentials.
func (h *Handler) SkillsIndex(c *echo.Context) error {
	list, err := h.Skills.Skills(c.Request().Context())
	if err != nil {
		return err
	}
	index := skillsIndex{Skills: make([]skillsIndexEntry, len(list))}
	for i, s := range list {
		e := skillsIndexEntry{Name: s.Name, Description: s.Description}
		for _, f := range s.Files {
			e.Files = append(e.Files, f.Path)
		}
		index.Skills[i] = e
	}
	return c.JSON(http.StatusOK, index)
}
