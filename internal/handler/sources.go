package handler

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/pkg/db/opensearch"
)

// ThreadSources lists the sources of a thread's conversation, by number.
func (h *Handler) ThreadSources(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	list, err := h.Store.Sources(ctx, rootID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(list))
}

// ThreadSource returns one source of a thread's conversation: what a
// [source:N] citation in the thread refers to.
func (h *Handler) ThreadSource(c *echo.Context) error {
	n, err := echo.PathParam[int](c, "n")
	if err != nil || n < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "a source number is a positive integer")
	}
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	s, err := h.Store.Source(ctx, rootID, n)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

// rootOf is the conversation a thread belongs to: sources and artifacts are
// kept per conversation, so a sub-agent's thread sees its parent's.
func (h *Handler) rootOf(ctx context.Context, threadID string) (string, error) {
	t, err := h.Store.Thread(ctx, threadID)
	if err != nil {
		return "", err
	}
	return t.RootID, nil
}

// recentSource is a source in the cross-conversation list, with the title of
// the conversation it belongs to.
type recentSource struct {
	opensearch.Source
	ThreadTitle string `json:"thread_title"`
}

// RecentSources lists sources across every conversation, newest first, paged
// as RecentArtifacts is (?limit=, ?after=<next>, ?workspace=), only those an
// answer or artifact cites when ?cited=true.
func (h *Handler) RecentSources(c *echo.Context) error {
	ctx := c.Request().Context()
	limit, err := echo.QueryParamOr[int](c, "limit", 0)
	if err != nil {
		return err
	}
	cited, err := echo.QueryParamOr[bool](c, "cited", false)
	if err != nil {
		return err
	}
	after, err := afterParam[opensearch.SourceKey](c)
	if err != nil {
		return err
	}
	roots, err := h.workspaceRoots(c)
	if err != nil {
		return err
	}
	found, next, err := h.Store.RecentSources(ctx, roots, cited, after, limit)
	if err != nil {
		return err
	}
	titles, err := threadTitles(ctx, h.Store, found, func(s opensearch.Source) string { return s.RootID })
	if err != nil {
		return err
	}
	items := make([]recentSource, 0, len(found))
	for _, s := range found {
		items = append(items, recentSource{Source: s, ThreadTitle: titles[s.RootID]})
	}
	return c.JSON(http.StatusOK, newPage(items, next))
}
