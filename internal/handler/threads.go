package handler

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/pkg/db/opensearch"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
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
	active, err := h.Service.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	items := make([]threadItem, 0, len(threads))
	for _, t := range threads {
		item := threadItem{Thread: t}
		if run, ok := active[t.ID]; ok {
			item.RunStatus = string(run.Status)
		}
		items = append(items, item)
	}
	return c.JSON(http.StatusOK, newList(items))
}

// Thread returns one thread.
func (h *Handler) Thread(c *echo.Context) error {
	ctx := c.Request().Context()
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if errors.Is(err, opensearch.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "thread not found")
	}
	if err != nil {
		return err
	}
	item := threadItem{Thread: *t}
	if active, err := h.Service.ActiveRuns(ctx); err == nil {
		if run, ok := active[t.ID]; ok {
			item.RunStatus = string(run.Status)
		}
	}
	return c.JSON(http.StatusOK, item)
}

// UpdateThread renames a thread.
func (h *Handler) UpdateThread(c *echo.Context) error {
	ctx := c.Request().Context()
	var body struct {
		Title string `json:"title"`
	}
	if err := c.Bind(&body); err != nil || body.Title == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "title is required")
	}
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if errors.Is(err, opensearch.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "thread not found")
	}
	if err != nil {
		return err
	}
	t.Title = body.Title
	if err := h.Store.SaveThread(ctx, t); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, t)
}

// DeleteThread removes a thread and its messages.
func (h *Handler) DeleteThread(c *echo.Context) error {
	if err := h.Store.DeleteThread(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// Messages pages a thread's messages, newest page first, and returns the log
// cursor to subscribe from: the active turn's start, or "" when idle.
func (h *Handler) Messages(c *echo.Context) error {
	ctx := c.Request().Context()
	before, _ := echo.QueryParamOr[int64](c, "before", 0)
	limit, _ := echo.QueryParamOr[int](c, "limit", defaultPageSize)
	if limit <= 0 || limit > maxPageSize {
		limit = defaultPageSize
	}
	msgs, err := h.Store.Messages(ctx, c.Param("id"), before, limit)
	if err != nil {
		return err
	}
	cursor := ""
	if active, err := h.Service.ActiveRuns(ctx); err == nil {
		if run, ok := active[c.Param("id")]; ok {
			cursor = run.TurnFrom
		}
	}
	resp := struct {
		list[opensearch.Message]
		Cursor string `json:"cursor"`
	}{newList(msgs), cursor}
	return c.JSON(http.StatusOK, resp)
}
