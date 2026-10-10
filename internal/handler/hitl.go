package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/hitl"
)

// Interrupts lists what the thread's paused run is waiting for.
func (h *Handler) Interrupts(c *echo.Context) error {
	pending, err := h.Service.Interrupts(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(pending))
}

// DecideInterrupt answers one interrupt over HTTP: `{approved, justification}`
// for a confirmation, `{output}` for a client-answered tool.
func (h *Handler) DecideInterrupt(c *echo.Context) error {
	var d hitl.Decision
	if err := c.Bind(&d); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid decision")
	}
	run, err := h.Service.Decide(c.Request().Context(), c.Param("id"), c.Param("iid"), d)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusAccepted, map[string]any{"run_id": run.ID, "cursor": run.SegmentFrom})
}
