package handler

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/agent"
)

// chatRequest is the chat-completions shape plus agentlite's fields. The last
// user message is the turn; its optional id makes a retry idempotent.
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	ThreadID   string    `json:"thread_id"`
	AgentID    string    `json:"agent_id"`
	Connectors *[]string `json:"connectors"`
}

// StartRun queues a turn and returns the run to follow over the socket from
// `cursor`. Without a thread_id it creates the thread, whose id comes back
// on the run.
func (h *Handler) StartRun(c *echo.Context) error {
	ctx := c.Request().Context()
	var req chatRequest
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request")
	}
	if len(req.Messages) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "messages are required")
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.Content == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "the last message must be from the user")
	}
	if req.Connectors != nil {
		if err := h.Config.CheckConnectors(*req.Connectors); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	if req.ThreadID == "" {
		thread, err := h.Service.CreateThread(ctx, req.Connectors)
		if err != nil {
			return err
		}
		req.ThreadID = thread.ID
	}
	run, err := h.Service.Start(ctx, agent.StartInput{
		ThreadID: req.ThreadID, AgentID: req.AgentID, Model: req.Model, Connectors: req.Connectors, MessageID: last.ID, Text: last.Content,
	})
	switch {
	case errors.Is(err, agent.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, agent.ErrInvalid):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.Is(err, agent.ErrBusy):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case err != nil:
		return err
	}
	return c.JSON(http.StatusAccepted, map[string]any{
		"id": run.ID, "object": "run", "thread_id": run.ThreadID, "agent_id": run.AgentID, "model": run.Model,
		"status": run.Status, "cursor": run.SegmentFrom,
	})
}

// CancelRun stops the thread's active run.
func (h *Handler) CancelRun(c *echo.Context) error {
	err := h.Service.Cancel(c.Request().Context(), c.Param("id"))
	if errors.Is(err, agent.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "no active run")
	}
	if err != nil {
		return err
	}
	return c.NoContent(http.StatusAccepted)
}
