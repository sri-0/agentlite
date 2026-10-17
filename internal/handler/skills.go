package handler

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"agentlite/internal/skills"
	"agentlite/pkg/db/opensearch"
)

// skillList is a listing of the library with the categories and sources of
// the skills ?q= matches, for filtering.
type skillList struct {
	list[opensearch.Skill]
	Categories []opensearch.Facet `json:"categories"`
	Sources    []opensearch.Facet `json:"sources"`
}

// ListSkills lists the skills library by name, each skill with its files:
// every skill, or those ?q= finds in their name, title or description, in
// ?category= and from ?source=.
func (h *Handler) ListSkills(c *echo.Context) error {
	got, err := h.Skills.List(c.Request().Context(), opensearch.SkillFilter{
		Q: c.QueryParam("q"), Category: c.QueryParam("category"), Source: c.QueryParam("source"),
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, skillList{list: newList(got.Skills), Categories: got.Categories, Sources: got.Sources})
}

// GetSkill returns one skill.
func (h *Handler) GetSkill(c *echo.Context) error {
	s, err := h.Skills.Get(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

// CreateSkill adds a skill to the library, replacing any of the same name: a
// multipart form whose file is a zip of the skill or its SKILL.md, with
// optional title, description, category and source. An empty field keeps
// what the skill has.
func (h *Handler) CreateSkill(c *echo.Context) error {
	f, _, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil: // no file, not multipart, a broken form
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the skill's zip or SKILL.md as file").Wrap(err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	files, err := skills.Read(body)
	if err != nil {
		return err
	}
	field := func(name string) *string {
		if v := strings.TrimSpace(c.FormValue(name)); v != "" {
			return &v
		}
		return nil
	}
	s, err := h.Skills.Save(c.Request().Context(), skills.Draft{Files: files, Meta: skills.Meta{
		Title: field("title"), Description: field("description"), Category: field("category"), Source: field("source"),
	}})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, s)
}

// UpdateSkill sets a skill's title, description, category or source:
// fields left out stay, "" clears a title, category or source.
func (h *Handler) UpdateSkill(c *echo.Context) error {
	var m skills.Meta
	if err := c.Bind(&m); err != nil || m == (skills.Meta{}) {
		return echo.NewHTTPError(http.StatusBadRequest, "send at least one of title, description, category, source")
	}
	s, err := h.Skills.Update(c.Request().Context(), c.Param("id"), m)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

// WriteSkillFile replaces one of a skill's files with the body, or adds it.
func (h *Handler) WriteSkillFile(c *echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err // the body limit's 413 among others
	}
	s, err := h.Skills.WriteFile(c.Request().Context(), c.Param("id"), c.Param("*"), body)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

// DeleteSkill removes a skill from the library.
func (h *Handler) DeleteSkill(c *echo.Context) error {
	if err := h.Skills.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// SkillFile serves one of a skill's files as written.
func (h *Handler) SkillFile(c *echo.Context) error {
	ctx := c.Request().Context()
	s, err := h.Skills.Get(ctx, c.Param("id"))
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
