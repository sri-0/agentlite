package handler

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"agentlite/pkg/db/opensearch"
)

// maxWorkspaceName bounds a workspace's name, which the sidebar shows whole.
const maxWorkspaceName = 80

// Workspaces lists the workspaces by name.
func (h *Handler) Workspaces(c *echo.Context) error {
	found, err := h.Store.Workspaces(c.Request().Context())
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(found))
}

// CreateWorkspace adds a workspace: {name}, in the control set X-Control-Set
// names.
func (h *Handler) CreateWorkspace(c *echo.Context) error {
	name, err := workspaceName(c)
	if err != nil {
		return err
	}
	now := time.Now()
	w := &opensearch.Workspace{ID: uuid.Must(uuid.NewV7()).String(), Name: name, CreatedAt: now, UpdatedAt: now}
	if err := h.Store.SaveWorkspace(c.Request().Context(), w); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, w)
}

// RenameWorkspace renames a workspace: {name}; only its creator may.
func (h *Handler) RenameWorkspace(c *echo.Context) error {
	ctx := c.Request().Context()
	name, err := workspaceName(c)
	if err != nil {
		return err
	}
	w, err := h.Store.Workspace(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if err := opensearch.CheckOwner(ctx, w.Labels); err != nil {
		return err
	}
	w.Name, w.UpdatedAt = name, time.Now()
	if err := h.Store.SaveWorkspace(ctx, w); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, w)
}

// DeleteWorkspace removes a workspace; its threads stay, outside any
// workspace. Only its creator may.
func (h *Handler) DeleteWorkspace(c *echo.Context) error {
	if err := h.Store.DeleteWorkspace(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// workspaceName is the request's {name}, trimmed; a blank or long one is 400.
func workspaceName(c *echo.Context) (string, error) {
	var body struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&body); err != nil {
		return "", err
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > maxWorkspaceName {
		return "", echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxWorkspaceName))
	}
	return name, nil
}
