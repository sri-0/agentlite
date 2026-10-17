package handler

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
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

// ListSkills lists the skills library by name, each skill with the paths of
// its files: every skill, or those ?q= finds in their name, title or
// description, in ?category= and from ?source=.
func (h *Handler) ListSkills(c *echo.Context) error {
	got, err := h.Skills.Skills(c.Request().Context(), opensearch.SkillFilter{
		Q: c.QueryParam("q"), Category: c.QueryParam("category"), Source: c.QueryParam("source"),
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, skillList{list: newList(got.Skills), Categories: got.Categories, Sources: got.Sources})
}

// CreateSkill adds a skill to the library, replacing any of the same name: a
// multipart form whose file is a zip of the skill or its SKILL.md, with
// optional title, description, category and source. A replaced skill keeps
// its title, category and source unless the form sets them.
func (h *Handler) CreateSkill(c *echo.Context) error {
	f, _, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, http.ErrMissingFile), errors.Is(err, http.ErrNotMultipart):
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the skill's zip or SKILL.md as file")
	case err != nil:
		return err // the body limit's 413 among others
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	s, err := skills.Read(body)
	if err != nil {
		return err
	}
	field := func(name string) *string { // an empty field keeps what the skill has
		if v := strings.TrimSpace(c.FormValue(name)); v != "" {
			return &v
		}
		return nil
	}
	m := skills.Meta{
		Title: field("title"), Description: field("description"), Category: field("category"), Source: field("source"),
	}
	if err := m.Apply(s); err != nil {
		return err
	}
	if err := h.Skills.Save(c.Request().Context(), s); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, withoutContent(s))
}

// UpdateSkill sets a skill's title, description, category or source:
// fields left out stay, "" clears a title, category or source.
func (h *Handler) UpdateSkill(c *echo.Context) error {
	var m skills.Meta
	if err := c.Bind(&m); err != nil || m == (skills.Meta{}) {
		return echo.NewHTTPError(http.StatusBadRequest, "send at least one of title, description, category, source")
	}
	s, err := h.Skills.Update(c.Request().Context(), c.Param("name"), m)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, withoutContent(s))
}

// withoutContent is s as the API returns it: its files' paths only.
func withoutContent(s *opensearch.Skill) *opensearch.Skill {
	for i := range s.Files {
		s.Files[i].Content = nil
	}
	return s
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
	list, err := h.Skills.Skills(c.Request().Context(), opensearch.SkillFilter{})
	if err != nil {
		return err
	}
	index := skillsIndex{Skills: make([]skillsIndexEntry, len(list.Skills))}
	for i, s := range list.Skills {
		e := skillsIndexEntry{Name: s.Name, Description: s.Description}
		for _, f := range s.Files {
			e.Files = append(e.Files, f.Path)
		}
		index.Skills[i] = e
	}
	return c.JSON(http.StatusOK, index)
}
