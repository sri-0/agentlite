package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// passThrough are the upstream endpoints proxied as they are; the provider is
// the one the body's model names (providers.Registry.Passthrough). Serving a
// new upstream endpoint is one more line here.
var passThrough = []string{
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/messages",
	"/v1/embeddings",
}

// RegisterProxies mounts the pass-through endpoints.
func (h *Handler) RegisterProxies(e *echo.Echo) {
	proxy := echo.WrapHandler(http.HandlerFunc(h.Registry.Passthrough))
	for _, path := range passThrough {
		e.POST(path, proxy)
	}
}
