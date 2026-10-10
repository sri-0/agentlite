package handler

import (
	"net/http"
	"net/url"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v5"

	"agentlite/internal/ws"
)

// Chat upgrades to the agent chat socket (see internal/ws).
func (h *Handler) Chat(c *echo.Context) error {
	if c.QueryParam("v") != ws.Version {
		return echo.NewHTTPError(http.StatusUpgradeRequired, "unsupported protocol version; use ?v="+ws.Version)
	}
	conn, err := websocket.Accept(c.Response(), c.Request(), &websocket.AcceptOptions{OriginPatterns: originHosts(h.Config.AllowedOrigins)})
	if err != nil {
		return nil // Accept has already written the HTTP error
	}
	ws.Serve(c.Request().Context(), conn, h.Service)
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
