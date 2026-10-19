package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// Interrupts lists what the thread's paused run is waiting for. Answers go
// over the socket (internal/ws). Interrupts are in Valkey, which knows
// nothing of control sets, so the thread must be readable first.
func (h *Handler) Interrupts(c *echo.Context) error {
	ctx := c.Request().Context()
	if _, err := h.Store.Thread(ctx, c.Param("id")); err != nil {
		return err
	}
	pending, err := h.Runs.Interrupts(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(pending))
}
