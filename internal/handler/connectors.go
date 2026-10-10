package handler

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/agent"
)

// Connectors lists the tool groups a thread can switch on.
func (h *Handler) Connectors(c *echo.Context) error {
	return c.JSON(http.StatusOK, newList(h.Engine.Connectors()))
}

// Connector describes one connector, with an MCP server's tools listed live.
func (h *Handler) Connector(c *echo.Context) error {
	info, err := h.Engine.Connector(c.Request().Context(), c.Param("id"))
	if errors.Is(err, agent.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, info)
}
