package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

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

// RecentArtifacts lists artifacts across every conversation, newest first,
// ?limit= at a time (default 50, at most 200). A full page carries next, an
// opaque cursor: ?after=<next> fetches the following page.
func (h *Handler) RecentArtifacts(c *echo.Context) error {
	ctx := c.Request().Context()
	limit, err := echo.QueryParamOr[int](c, "limit", 0)
	if err != nil {
		return err
	}
	var after *opensearch.ArtifactKey
	if v := c.QueryParam("after"); v != "" {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err == nil {
			err = json.Unmarshal(raw, &after)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "after must be a next cursor from this list")
		}
	}
	found, next, err := h.Store.RecentArtifacts(ctx, after, limit)
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
	page := artifactsPage{list: newList(items)}
	if next != nil {
		key, _ := json.Marshal(next)
		page.Next = base64.RawURLEncoding.EncodeToString(key)
	}
	return c.JSON(http.StatusOK, page)
}

// artifactsPage is one page of RecentArtifacts.
type artifactsPage struct {
	list[recentArtifact]
	Next string `json:"next,omitempty"`
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

// ArtifactContent serves an artifact's body from S3, the latest version or
// ?v='s; its route sandboxes it.
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
	// ?v= serves an earlier version, which never changes; every version's
	// body is kept.
	if v, _ := strconv.Atoi(c.QueryParam("v")); v > 0 && v <= a.Version {
		a.Version = v
		c.Response().Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	}
	body, err := h.Artifacts.Body(ctx, a)
	if err != nil {
		return err
	}
	defer body.Close()
	return c.Stream(http.StatusOK, artifacts.ContentType(a), body)
}
