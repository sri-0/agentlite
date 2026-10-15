package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"agentlite/pkg/modelcatalog"
)

// maxPassThroughBody bounds a pass-through request, which is read whole to
// find its model.
const maxPassThroughBody = 32 << 20

// passThrough are the upstream endpoints proxied as they are, each with the
// model types it takes; the provider is the one the body's model names.
// Serving a new upstream endpoint is one more line here.
var passThrough = map[string][]string{
	// Image models generate and edit through chat too (vLLM-Omni, OpenRouter),
	// and audio models (Voxtral) hear through it.
	"/v1/chat/completions": {modelcatalog.TypeLLM, modelcatalog.TypeImage, modelcatalog.TypeAudio},
	"/v1/responses":        {modelcatalog.TypeLLM},
	"/v1/messages":         {modelcatalog.TypeLLM}, // Anthropic's API
	"/v1/embeddings":       {modelcatalog.TypeEmbedding},
	"/v1/decisions":        {modelcatalog.TypeDecision}, // OpenAI's decisions API
	"/v1/systemone":        {modelcatalog.TypeDecision}, // TypeSafe's System One API
}

// RegisterProxies mounts the pass-through endpoints.
func (h *Handler) RegisterProxies(e *echo.Echo) {
	limit := middleware.BodyLimit(maxPassThroughBody)
	for path, types := range passThrough {
		e.POST(path, h.passThrough(types), limit)
	}
}

// passThrough forwards a request to the provider its body's "model" names,
// with the model rewritten to the provider's own id; everything else passes
// as it is, streamed responses included. A model that names no known provider
// or model, or a model of a type the endpoint does not take, is refused and
// never sent elsewhere.
func (h *Handler) passThrough(types []string) echo.HandlerFunc {
	return func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err // the body limit's 413, or a client that went away
		}
		var fields map[string]json.RawMessage
		var model string
		if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
			return echo.NewHTTPError(http.StatusBadRequest, `the body must be a JSON object with a "model" naming provider/model`)
		}
		t, err := h.Registry.Resolve(model)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		if !slices.Contains(types, t.Model.Type) {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s takes %s models; %q is of type %s",
				c.Request().URL.Path, strings.Join(types, " or "), model, t.Model.Type))
		}
		fields["model"], _ = json.Marshal(t.Model.UpstreamID)
		body, _ = json.Marshal(fields)
		if err := h.Registry.Forward(c.Response(), c.Request(), t, body); err != nil {
			return echo.NewHTTPError(http.StatusBadGateway, "the provider could not be reached").Wrap(err)
		}
		return nil
	}
}
