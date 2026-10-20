// Package handler holds the HTTP and WebSocket handlers.
package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"

	"agentlite/config"
	"agentlite/internal/agents"
	"agentlite/internal/artifacts"
	"agentlite/internal/providers"
	"agentlite/internal/rag/userfiles"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/internal/suggest"
	"agentlite/internal/voice"
	"agentlite/pkg/auth"
	"agentlite/pkg/db/opensearch"
)

// Handler carries the dependencies every route needs.
type Handler struct {
	Config    *config.Config
	Auth      *auth.Verifier
	Registry  *providers.Registry
	Runs      *runs.Manager
	Runtime   *agents.Runtime
	Store     *opensearch.Client
	Artifacts *artifacts.Store
	Skills    *skills.Library
	Files     *userfiles.Store
	Voice     *voice.Transcriber // nil when the voice prompt is not configured
	Suggest   *suggest.Suggester
	Log       zerolog.Logger
}

// list is the OpenAI-style list envelope the UI expects.
type list[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`
}

func newList[T any](items []T) list[T] {
	if items == nil {
		items = []T{}
	}
	return list[T]{Object: "list", Data: items}
}

// page is one page of a list across conversations; a full one carries next,
// an opaque cursor for ?after= (see afterParam).
type page[T any] struct {
	list[T]
	Next string `json:"next,omitempty"`
}

// newPage is a page of items, next the store's key of the following one
// (nil: this was the last).
func newPage[T, K any](items []T, next *K) page[T] {
	p := page[T]{list: newList(items)}
	if next != nil {
		key, _ := json.Marshal(next)
		p.Next = base64.RawURLEncoding.EncodeToString(key)
	}
	return p
}

// afterParam is ?after=, a next cursor from a page of the same list, as the
// store's key; nil when it is not set.
func afterParam[K any](c *echo.Context) (*K, error) {
	v := c.QueryParam("after")
	if v == "" {
		return nil, nil
	}
	var after *K
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err == nil {
		err = json.Unmarshal(raw, &after)
	}
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "after must be a next cursor from this list")
	}
	return after, nil
}

// roots is the conversations a list across them draws on: those filed under
// ?workspace=, or, when it is not set, every one the caller can see (their
// own and the public ones), so nothing of a private conversation is listed.
func (h *Handler) roots(c *echo.Context) ([]string, error) {
	ctx := c.Request().Context()
	if id := c.QueryParam("workspace"); id != "" {
		return h.Store.WorkspaceThreads(ctx, id)
	}
	return h.Store.RootIDs(ctx)
}

// threadTitles is the titles of the conversations items belong to, by root
// id (rootOf gives an item's).
func threadTitles[T any](ctx context.Context, store *opensearch.Client, items []T, rootOf func(T) string) (map[string]string, error) {
	roots := make([]string, 0, len(items))
	for _, it := range items {
		roots = append(roots, rootOf(it))
	}
	return store.ThreadTitles(ctx, slices.Compact(slices.Sorted(slices.Values(roots))))
}

// Health reports liveness and the registered agent ids.
func (h *Handler) Health(c *echo.Context) error {
	ids := make([]string, 0, len(h.Config.Agents))
	for _, a := range h.Config.Agents {
		ids = append(ids, a.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{"status": "ok", "agents": ids})
}
