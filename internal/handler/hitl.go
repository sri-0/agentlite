package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// Interrupts lists what the thread's paused run is waiting for. Answers go
// over the socket (internal/ws).
func (h *Handler) Interrupts(c *echo.Context) error {
	pending, err := h.Runs.Interrupts(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(pending))
}
