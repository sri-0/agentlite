package handler

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"agentlite/pkg/db/opensearch"
)

// maxWorkspaceName bounds a workspace's name, which the sidebar shows whole.
const maxWorkspaceName = 80

// workspaceItem is a workspace as the API serves it: with whether the
// caller created it, and so may change it.
type workspaceItem struct {
	opensearch.Workspace
	Mine bool `json:"mine"`
}

// workspaceBody is what a workspace is created or changed with: its name,
// and whether it is public (everyone who holds its control set sees it and
// files under it) or the creator's alone.
type workspaceBody struct {
	Name     *string `json:"name"`
	IsShared *bool   `json:"is_shared"`
}

// Workspaces lists the workspaces the caller can see, their own and the
// public ones, by name.
func (h *Handler) Workspaces(c *echo.Context) error {
	ctx := c.Request().Context()
	found, err := h.Store.Workspaces(ctx)
	if err != nil {
		return err
	}
	items := make([]workspaceItem, 0, len(found))
	for _, w := range found {
		items = append(items, workspaceItem{Workspace: w, Mine: opensearch.Mine(ctx, w.Marks)})
	}
	return c.JSON(http.StatusOK, newList(items))
}

// CreateWorkspace adds a workspace: {name, is_shared?}, public unless
// is_shared is false, in the control set X-Control-Set names.
func (h *Handler) CreateWorkspace(c *echo.Context) error {
	var body workspaceBody
	if err := c.Bind(&body); err != nil {
		return err
	}
	name, err := workspaceName(body.Name)
	if err != nil {
		return err
	}
	w := &opensearch.Workspace{ID: uuid.Must(uuid.NewV7()).String(), Name: name,
		Sharing: opensearch.Sharing{IsShared: body.IsShared == nil || *body.IsShared}}
	if err := h.Store.SaveWorkspace(c.Request().Context(), w); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, workspaceItem{Workspace: *w, Mine: true})
}

// UpdateWorkspace renames a workspace or changes who sees it: {name?,
// is_shared?}; only its creator may.
func (h *Handler) UpdateWorkspace(c *echo.Context) error {
	ctx := c.Request().Context()
	var body workspaceBody
	if err := c.Bind(&body); err != nil {
		return err
	}
	if body.Name == nil && body.IsShared == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "name or is_shared is required")
	}
	w, err := h.Store.Workspace(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if err := opensearch.CheckOwner(ctx, w.Marks); err != nil {
		return err
	}
	if body.Name != nil {
		if w.Name, err = workspaceName(body.Name); err != nil {
			return err
		}
	}
	if body.IsShared != nil {
		w.IsShared = *body.IsShared
	}
	if err := h.Store.SaveWorkspace(ctx, w); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, workspaceItem{Workspace: *w, Mine: true})
}

// DeleteWorkspace removes a workspace; its threads stay, outside any
// workspace. Only its creator may.
func (h *Handler) DeleteWorkspace(c *echo.Context) error {
	if err := h.Store.DeleteWorkspace(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// workspaceName is a workspace's name as given, trimmed; none, a blank or a
// long one is 400.
func workspaceName(name *string) (string, error) {
	if name == nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	n := strings.TrimSpace(*name)
	if n == "" || utf8.RuneCountInString(n) > maxWorkspaceName {
		return "", echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxWorkspaceName))
	}
	return n, nil
}
