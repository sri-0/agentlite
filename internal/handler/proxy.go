package handler

import (
	"github.com/labstack/echo/v5"

	"agentlite/internal/proxy"
)

// proxyRoutes maps the pass-through paths to their route names.
var proxyRoutes = map[string]string{
	"/v1/chat/completions": proxy.RouteChatCompletions,
	"/v1/responses":        proxy.RouteResponses,
	"/v1/messages":         proxy.RouteMessages,
	"/v1/embeddings":       proxy.RouteEmbeddings,
}

// RegisterProxies mounts every pass-through route on its upstream.
func (h *Handler) RegisterProxies(e *echo.Echo) error {
	for path, route := range proxyRoutes {
		client, err := h.Registry.ProxyFor(route)
		if err != nil {
			return err
		}
		rp := proxy.NewReverseProxy(client, h.Log)
		e.POST(path, func(c *echo.Context) error {
			rp.ServeHTTP(c.Response(), c.Request())
			return nil
		})
	}
	return nil
}
