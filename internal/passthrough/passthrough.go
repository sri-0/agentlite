// Package passthrough proxies the providers' own endpoints: a request goes to
// the provider its body's model names, with the model rewritten to the
// provider's own id, and everything else passes as it is, streamed responses
// included (providers.Registry.Forward). A model that names no known provider
// or model, or one of a type the endpoint does not take, is refused and never
// sent elsewhere.
package passthrough

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
)

// routes are the endpoints whose body is JSON, each with the model types it
// takes. Serving a new upstream endpoint is one more line here.
var routes = map[string][]string{
	// Image models generate and edit through chat too (vLLM-Omni, OpenRouter),
	// and audio models (Voxtral) hear through it.
	"/v1/chat/completions":   {modelcatalog.TypeLLM, modelcatalog.TypeImage, modelcatalog.TypeAudio},
	"/v1/responses":          {modelcatalog.TypeLLM},
	"/v1/messages":           {modelcatalog.TypeLLM}, // Anthropic's API
	"/v1/embeddings":         {modelcatalog.TypeEmbedding},
	"/v1/images/generations": {modelcatalog.TypeImage},
	"/v1/decisions":          {modelcatalog.TypeDecision}, // OpenAI's decisions API
	"/v1/systemone":          {modelcatalog.TypeDecision}, // TypeSafe's System One API
}

// Register mounts the endpoints: the JSON ones, and /v1/images/edits, whose
// body is a multipart form (vLLM-Omni takes the image as an upload). A body
// is read whole to find its model, within the server's body limit
// (MAX_BODY_MB).
func Register(e *echo.Echo, registry *providers.Registry) {
	for path, types := range routes {
		e.POST(path, func(c *echo.Context) error { return proxyJSON(c, registry, types) })
	}
	e.POST("/v1/images/edits", func(c *echo.Context) error { return proxyForm(c, registry) })
}

// proxyJSON forwards a JSON object with its "model" set to the provider's id.
func proxyJSON(c *echo.Context, registry *providers.Registry, types []string) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err // the body limit's 413, or a client that went away
	}
	var fields map[string]json.RawMessage
	var model string
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
		return echo.NewHTTPError(http.StatusBadRequest, `the body must be a JSON object with a "model" naming provider/model`)
	}
	t, err := resolve(c, registry, model, types)
	if err != nil {
		return err
	}
	fields["model"], _ = json.Marshal(t.Model.UpstreamID)
	body, _ = json.Marshal(fields)
	return forward(c, registry, t, body)
}

// proxyForm forwards a multipart form with its "model" field set to the
// provider's id; every other field and file goes as it came (each file part
// with its own headers), in a form the request's Content-Type now names.
func proxyForm(c *echo.Context, registry *providers.Registry) error {
	form, err := c.MultipartForm()
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil || len(form.Value["model"]) != 1:
		return echo.NewHTTPError(http.StatusBadRequest, `the body must be a multipart form with a "model" naming provider/model`).Wrap(err)
	}
	defer form.RemoveAll()
	t, err := resolve(c, registry, form.Value["model"][0], []string{modelcatalog.TypeImage})
	if err != nil {
		return err
	}
	form.Value["model"] = []string{t.Model.UpstreamID}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for name, values := range form.Value {
		for _, v := range values {
			_ = w.WriteField(name, v) // into a buffer: no errors
		}
	}
	for _, files := range form.File {
		for _, fh := range files {
			f, err := fh.Open()
			if err != nil {
				return err
			}
			part, _ := w.CreatePart(fh.Header)
			_, err = io.Copy(part, f)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	_ = w.Close()
	c.Request().Header.Set("Content-Type", w.FormDataContentType())
	return forward(c, registry, t, body.Bytes())
}

// resolve is the model a request names, if it is of a type the endpoint takes.
func resolve(c *echo.Context, registry *providers.Registry, model string, types []string) (providers.Target, error) {
	t, err := registry.ResolveAs(model, types...)
	if err != nil {
		return t, echo.NewHTTPError(http.StatusBadRequest, c.Path()+": "+err.Error())
	}
	return t, nil
}

func forward(c *echo.Context, registry *providers.Registry, t providers.Target, body []byte) error {
	if err := registry.Forward(c.Response(), c.Request(), t, body); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "the provider could not be reached").Wrap(err)
	}
	return nil
}
