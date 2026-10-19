package handler

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/runs"
	"agentlite/pkg/db/opensearch"
)

type threadItem struct {
	opensearch.Thread
	RunStatus string `json:"run_status,omitempty"`
}

// Threads lists root threads with the status of any active run.
func (h *Handler) Threads(c *echo.Context) error {
	ctx := c.Request().Context()
	threads, err := h.Store.Threads(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, len(threads))
	for i, t := range threads {
		ids[i] = t.ID
	}
	statuses, err := h.Runs.RunStatuses(ctx, ids)
	if err != nil {
		return err
	}
	items := make([]threadItem, 0, len(threads))
	for _, t := range threads {
		items = append(items, threadItem{Thread: t, RunStatus: string(statuses[t.ID])})
	}
	return c.JSON(http.StatusOK, newList(items))
}

// Thread returns one thread.
func (h *Handler) Thread(c *echo.Context) error {
	ctx := c.Request().Context()
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	item := threadItem{Thread: *t}
	run, err := h.Runs.Active(ctx, t.ID)
	if err != nil {
		return err
	}
	if run != nil {
		item.RunStatus = string(run.Status)
	}
	return c.JSON(http.StatusOK, item)
}

// UpdateThread renames a thread, changes its connectors, or moves it to a
// workspace ("" takes it out of its workspace).
func (h *Handler) UpdateThread(c *echo.Context) error {
	ctx := c.Request().Context()
	var body struct {
		Title       string    `json:"title"`
		Connectors  *[]string `json:"connectors"`
		WorkspaceID *string   `json:"workspace_id"`
	}
	if err := c.Bind(&body); err != nil {
		return err
	}
	if body.Title == "" && body.Connectors == nil && body.WorkspaceID == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "title, connectors or workspace_id is required")
	}
	if body.Connectors != nil {
		if err := h.Runtime.CheckConnectors(*body.Connectors); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	if body.WorkspaceID != nil {
		if err := h.Store.CheckWorkspace(ctx, *body.WorkspaceID); err != nil {
			return err
		}
	}
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if body.WorkspaceID != nil && t.ParentID != "" {
		return fmt.Errorf("%w: a sub-agent's thread is filed with its conversation; move %s", runs.ErrInvalid, t.RootID)
	}
	if body.Title != "" {
		t.Title = body.Title
	}
	if body.Connectors != nil {
		t.Connectors = body.Connectors
	}
	if body.WorkspaceID != nil {
		t.WorkspaceID = *body.WorkspaceID
	}
	if err := h.Store.SaveThread(ctx, t); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, t)
}

// DeleteThread removes a conversation: its root and sub-agent threads, their
// messages and memories, and what Valkey keeps of them. Its sources and
// artifacts stay as evidence. A thread with an active run is refused.
func (h *Handler) DeleteThread(c *echo.Context) error {
	ctx, id := c.Request().Context(), c.Param("id")
	t, err := h.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if t.ParentID != "" {
		return fmt.Errorf("%w: a sub-agent's thread goes with its conversation; delete %s", runs.ErrInvalid, t.RootID)
	}
	run, err := h.Runs.Active(ctx, id)
	if err != nil {
		return err
	}
	if run != nil {
		return runs.ErrBusy
	}
	subs, err := h.Store.SubThreads(ctx, id)
	if err != nil {
		return err
	}
	if err := h.Runs.Forget(ctx, append(subs, id)...); err != nil {
		return err
	}
	if err := h.Store.DeleteThread(ctx, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// Messages pages a thread's messages, newest page first, and returns the log
// cursor to subscribe from: the active turn's start, or "" when idle.
func (h *Handler) Messages(c *echo.Context) error {
	ctx := c.Request().Context()
	before, err := echo.QueryParamOr[int64](c, "before", 0)
	if err != nil {
		return err
	}
	limit, err := echo.QueryParamOr[int](c, "limit", 0)
	if err != nil {
		return err
	}
	msgs, err := h.Store.Messages(ctx, c.Param("id"), before, limit)
	if err != nil {
		return err
	}
	cursor := ""
	run, err := h.Runs.Active(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if run != nil {
		cursor = run.TurnFrom
	}
	resp := struct {
		list[opensearch.Message]
		Cursor string `json:"cursor"`
	}{newList(msgs), cursor}
	return c.JSON(http.StatusOK, resp)
}
