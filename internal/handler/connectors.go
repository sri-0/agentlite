package handler

import (
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Connectors lists the tool groups a thread can switch on.
func (h *Handler) Connectors(c *echo.Context) error {
	return c.JSON(http.StatusOK, newList(h.Runtime.Connectors()))
}

// Connector describes one connector, with an MCP server's tools listed live.
func (h *Handler) Connector(c *echo.Context) error {
	info, err := h.Runtime.Connector(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, info)
}

// MCPProxy passes a client's MCP requests to an exposed connector's server
// (mcp.expose), such as opencode's from /.well-known/opencode.
func (h *Handler) MCPProxy(c *echo.Context) error {
	srv := h.Runtime.ExposedMCP(c.Param("id"))
	if srv == nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no exposed MCP server %q", c.Param("id")))
	}
	if err := srv.Forward(c.Response(), c.Request()); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "the MCP server could not be reached").Wrap(err)
	}
	return nil
}
