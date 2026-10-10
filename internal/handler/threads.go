package handler

import (
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
	ids := make([]string, len(threads))
	for i, t := range threads {
		ids[i] = t.ID
	}
	statuses, err := h.Service.RunStatuses(ctx, ids)
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
	run, err := h.Service.Active(ctx, t.ID)
	if err != nil {
		return err
	}
	if run != nil {
		item.RunStatus = string(run.Status)
	}
	return c.JSON(http.StatusOK, item)
}

// UpdateThread renames a thread or changes its connectors.
func (h *Handler) UpdateThread(c *echo.Context) error {
	ctx := c.Request().Context()
	var body struct {
		Title      string    `json:"title"`
		Connectors *[]string `json:"connectors"`
	}
	if err := c.Bind(&body); err != nil || (body.Title == "" && body.Connectors == nil) {
		return echo.NewHTTPError(http.StatusBadRequest, "title or connectors is required")
	}
	if body.Connectors != nil {
		if err := h.Config.CheckConnectors(*body.Connectors); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if body.Title != "" {
		t.Title = body.Title
	}
	if body.Connectors != nil {
		t.Connectors = body.Connectors
	}
	if err := h.Store.SaveThread(ctx, t); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, t)
}

// DeleteThread removes a root thread, its sub-agent threads and all their
// messages. A thread with an active run is refused.
func (h *Handler) DeleteThread(c *echo.Context) error {
	ctx := c.Request().Context()
	run, err := h.Service.Active(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	if run != nil {
		return echo.NewHTTPError(http.StatusConflict, "thread has an active run")
	}
	if err := h.Store.DeleteThread(ctx, c.Param("id")); err != nil {
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
	run, err := h.Service.Active(ctx, c.Param("id"))
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
