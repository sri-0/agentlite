package handler

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/runs"
	"agentlite/pkg/db/opensearch"
)

// threadItem is a thread as the API serves it: with the status of its
// active run, and whether the caller created it (and so may change it and
// take turns on it; anyone else reads a public thread).
type threadItem struct {
	opensearch.Thread
	RunStatus string `json:"run_status,omitempty"`
	Mine      bool   `json:"mine"`
}

// Threads lists root threads with the status of any active run: the
// caller's own (the sidebar), or, with ?workspace=, those filed under a
// workspace that the caller can see, their own and the public ones.
func (h *Handler) Threads(c *echo.Context) error {
	ctx := c.Request().Context()
	filter := opensearch.ThreadFilter{Mine: true}
	if w := c.QueryParam("workspace"); w != "" {
		filter = opensearch.ThreadFilter{WorkspaceID: w}
	}
	threads, err := h.Store.Threads(ctx, filter)
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
		items = append(items, threadItem{Thread: t, RunStatus: string(statuses[t.ID]), Mine: opensearch.Mine(ctx, t.Marks)})
	}
	return c.JSON(http.StatusOK, newList(items))
}

// Thread returns one thread: the caller's own, or a public one, by its id
// (the share link names it).
func (h *Handler) Thread(c *echo.Context) error {
	ctx := c.Request().Context()
	t, err := h.Store.Thread(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	item := threadItem{Thread: *t, Mine: opensearch.Mine(ctx, t.Marks)}
	run, err := h.Runs.Active(ctx, t.ID)
	if err != nil {
		return err
	}
	if run != nil {
		item.RunStatus = string(run.Status)
	}
	return c.JSON(http.StatusOK, item)
}

// UpdateThread renames a thread, changes its connectors, moves it to a
// workspace ("" takes it out of its workspace), pins or archives it, or
// shares it (is_shared: public, readable by its link by everyone who holds
// its control set, or the creator's alone; its sub-agent threads follow);
// only its creator may.
func (h *Handler) UpdateThread(c *echo.Context) error {
	ctx := c.Request().Context()
	var body struct {
		Title       string    `json:"title"`
		Connectors  *[]string `json:"connectors"`
		WorkspaceID *string   `json:"workspace_id"`
		Pinned      *bool     `json:"pinned"`
		Archived    *bool     `json:"archived"`
		IsShared    *bool     `json:"is_shared"`
	}
	if err := c.Bind(&body); err != nil {
		return err
	}
	if body.Title == "" && body.Connectors == nil && body.WorkspaceID == nil && body.Pinned == nil && body.Archived == nil && body.IsShared == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "title, connectors, workspace_id, pinned, archived or is_shared is required")
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
	if err := opensearch.CheckOwner(ctx, t.Marks); err != nil {
		return err
	}
	if body.WorkspaceID != nil && t.ParentID != "" {
		return fmt.Errorf("%w: a sub-agent's thread is filed with its conversation; move %s", runs.ErrInvalid, t.RootID)
	}
	if body.IsShared != nil && t.ParentID != "" {
		return fmt.Errorf("%w: a sub-agent's thread is shared with its conversation; share %s", runs.ErrInvalid, t.RootID)
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
	if body.Pinned != nil {
		t.Pinned = *body.Pinned
	}
	if body.Archived != nil {
		t.Archived = *body.Archived
	}
	if body.IsShared != nil {
		t.IsShared = *body.IsShared
	}
	if err := h.Store.SaveThread(ctx, t); err != nil {
		return err
	}
	if body.IsShared != nil {
		if err := h.Store.ShareSubThreads(ctx, t.ID, t.IsShared); err != nil {
			return err
		}
	}
	return c.JSON(http.StatusOK, threadItem{Thread: *t, Mine: true})
}

// DeleteThread removes a conversation: its root and sub-agent threads, their
// messages and memories, and what Valkey keeps of them. Its sources and
// artifacts stay as evidence. Only its creator may; a thread with an active
// run is refused.
func (h *Handler) DeleteThread(c *echo.Context) error {
	ctx, id := c.Request().Context(), c.Param("id")
	t, err := h.Store.Thread(ctx, id)
	if err != nil {
		return err
	}
	if err := opensearch.CheckOwner(ctx, t.Marks); err != nil {
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
// cursor to subscribe from: the active turn's start, or "" when idle. The
// cursor is in Valkey, which knows nothing of control sets, so the thread
// must be readable first.
func (h *Handler) Messages(c *echo.Context) error {
	ctx := c.Request().Context()
	if _, err := h.Store.Thread(ctx, c.Param("id")); err != nil {
		return err
	}
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
