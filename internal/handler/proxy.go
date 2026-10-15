package handler

import (
	"github.com/labstack/echo/v5"

	"agentlite/pkg/modelcatalog"
)

// passThrough are the upstream endpoints proxied as they are, each with the
// model types it takes; the provider is the one the body's model names
// (providers.Registry.Passthrough). Serving a new upstream endpoint is one
// more line here.
var passThrough = map[string][]string{
	"/v1/chat/completions": {modelcatalog.TypeLLM},
	"/v1/responses":        {modelcatalog.TypeLLM},
	"/v1/messages":         {modelcatalog.TypeLLM}, // Anthropic's API
	"/v1/embeddings":       {modelcatalog.TypeEmbedding},
	"/v1/decisions":        {modelcatalog.TypeDecision}, // OpenAI's decisions API
	"/v1/systemone":        {modelcatalog.TypeDecision}, // TypeSafe's System One API
}

// RegisterProxies mounts the pass-through endpoints.
func (h *Handler) RegisterProxies(e *echo.Echo) {
	for path, types := range passThrough {
		e.POST(path, echo.WrapHandler(h.Registry.Passthrough(types...)))
	}
}
