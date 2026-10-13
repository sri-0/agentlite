package handler

import (
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v5"

	"agentlite/internal/artifacts"
	"agentlite/pkg/db/opensearch"
)

// recentArtifact is an artifact in the cross-conversation list, with the title
// of the conversation it belongs to.
type recentArtifact struct {
	opensearch.Artifact
	ThreadTitle string `json:"thread_title"`
}

// RecentArtifacts lists artifacts across every conversation, newest first:
// ?before=<RFC 3339 time> pages back, ?limit= (default 50, at most 200).
func (h *Handler) RecentArtifacts(c *echo.Context) error {
	ctx := c.Request().Context()
	limit, _ := echo.QueryParamOr[int](c, "limit", defaultPageSize)
	if limit <= 0 || limit > maxPageSize {
		limit = defaultPageSize
	}
	var before time.Time
	if v := c.QueryParam("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "before must be an RFC 3339 time")
		}
		before = t
	}
	found, err := h.Store.RecentArtifacts(ctx, before, limit)
	if err != nil {
		return err
	}
	roots := make([]string, 0, len(found))
	for _, a := range found {
		roots = append(roots, a.RootID)
	}
	titles, err := h.Store.ThreadTitles(ctx, slices.Compact(slices.Sorted(slices.Values(roots))))
	if err != nil {
		return err
	}
	items := make([]recentArtifact, 0, len(found))
	for _, a := range found {
		items = append(items, recentArtifact{Artifact: a, ThreadTitle: titles[a.RootID]})
	}
	return c.JSON(http.StatusOK, newList(items))
}

// ThreadArtifacts lists the artifacts of a thread's conversation, newest
// first, without their bodies.
func (h *Handler) ThreadArtifacts(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	list, err := h.Store.Artifacts(ctx, rootID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(list))
}

// ArtifactContent serves an artifact's body, from S3 or the record.
func (h *Handler) ArtifactContent(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	a, err := h.Store.Artifact(ctx, rootID, c.Param("artifactId"))
	if err != nil {
		return err
	}
	body, err := h.Artifacts.Body(ctx, a)
	if err != nil {
		return err
	}
	defer body.Close()
	// A body is model-written: opened directly, an HTML one must not run as this origin.
	c.Response().Header().Set("Content-Security-Policy", "sandbox")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Stream(http.StatusOK, artifacts.ContentType(a.Kind), body)
}
