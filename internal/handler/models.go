package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/config"
	"agentlite/pkg/modelcatalog"
)

type modelItem struct {
	Object string `json:"object"`
	modelcatalog.Model
}

// Models serves the merged model list from every provider.
func (h *Handler) Models(c *echo.Context) error {
	models := h.Registry.Models()
	items := make([]modelItem, 0, len(models))
	for _, m := range models {
		items = append(items, modelItem{Object: "model", Model: m})
	}
	return c.JSON(http.StatusOK, newList(items))
}

// Agents serves the primary agents (those a user may pick).
func (h *Handler) Agents(c *echo.Context) error {
	var items []config.AgentDef
	for _, a := range h.Config.Agents {
		if a.Primary() {
			items = append(items, a)
		}
	}
	return c.JSON(http.StatusOK, newList(items))
}
