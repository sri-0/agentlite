// Package handler holds the HTTP and WebSocket handlers.
package handler

import (
	"net/http"

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
	"agentlite/pkg/db/opensearch"
)

// Handler carries the dependencies every route needs.
type Handler struct {
	Config    *config.Config
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

// Health reports liveness and the registered agent ids.
func (h *Handler) Health(c *echo.Context) error {
	ids := make([]string, 0, len(h.Config.Agents))
	for _, a := range h.Config.Agents {
		ids = append(ids, a.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{"status": "ok", "agents": ids})
}
