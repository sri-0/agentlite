package handler

import (
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"agentlite/internal/ws"
)

// startedThread is the reply to StartThread: the new thread, its run, and the
// log cursor to subscribe from.
type startedThread struct {
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
	Seq      string `json:"seq"`
}

// StartThread creates a thread with its first turn and queues the run. It
// replies once the thread is stored, so a subscribe sent on the reply finds
// it. Later turns on the thread go over the socket.
func (h *Handler) StartThread(c *echo.Context) error {
	var turn ws.Turn
	if err := c.Bind(&turn); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	in, err := turn.StartInput("")
	if err != nil {
		return err
	}
	run, err := h.Runs.Start(c.Request().Context(), in)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, startedThread{ThreadID: run.ThreadID, RunID: run.ID, Seq: run.SegmentFrom})
}

// Chat upgrades to the agent chat socket (see internal/ws).
func (h *Handler) Chat(c *echo.Context) error {
	if c.QueryParam("v") != ws.Version {
		return echo.NewHTTPError(http.StatusUpgradeRequired, "unsupported protocol version; use ?v="+ws.Version)
	}
	conn, err := websocket.Accept(c.Response(), c.Request(), &websocket.AcceptOptions{OriginPatterns: originHosts(h.Config.AllowedOrigins)})
	if err != nil {
		return nil // Accept has already written the HTTP error
	}
	ws.Serve(c.Request().Context(), conn, h.Runs, h.Store)
	return nil
}

func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}
