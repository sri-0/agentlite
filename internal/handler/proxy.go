package handler

import (
	"github.com/labstack/echo/v5"

	"agentlite/internal/proxy"
)

// Proxy returns a handler that forwards the request to the route's upstream.
func (h *Handler) Proxy(route string) echo.HandlerFunc {
	client, err := h.Registry.ProxyFor(route)
	if err != nil {
		h.Log.Fatal().Err(err).Msg("proxy configuration")
	}
	rp := proxy.NewReverseProxy(client, h.Log)
	return func(c *echo.Context) error {
		rp.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

// proxyRoutes maps the pass-through paths to their route names.
var proxyRoutes = map[string]string{
	"/v1/chat/completions": proxy.RouteChatCompletions,
	"/v1/responses":        proxy.RouteResponses,
	"/v1/messages":         proxy.RouteMessages,
	"/v1/embeddings":       proxy.RouteEmbeddings,
}

// RegisterProxies mounts every pass-through route.
func (h *Handler) RegisterProxies(e *echo.Echo) {
	for path, route := range proxyRoutes {
		e.POST(path, h.Proxy(route))
	}
}
