package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"github.com/labstack/echo/v5"

	"agentlite/pkg/modelcatalog"
)

// passThrough are the upstream endpoints proxied as they are, each with the
// model types it takes; the provider is the one the body's model names.
// Serving a new upstream endpoint is one more line here.
var passThrough = map[string][]string{
	// Image models generate and edit through chat too (vLLM-Omni, OpenRouter),
	// and audio models (Voxtral) hear through it.
	"/v1/chat/completions":   {modelcatalog.TypeLLM, modelcatalog.TypeImage, modelcatalog.TypeAudio},
	"/v1/responses":          {modelcatalog.TypeLLM},
	"/v1/messages":           {modelcatalog.TypeLLM}, // Anthropic's API
	"/v1/embeddings":         {modelcatalog.TypeEmbedding},
	"/v1/images/generations": {modelcatalog.TypeImage},
	"/v1/images/edits":       {modelcatalog.TypeImage},    // a multipart form (vLLM-Omni)
	"/v1/decisions":          {modelcatalog.TypeDecision}, // OpenAI's decisions API
	"/v1/systemone":          {modelcatalog.TypeDecision}, // TypeSafe's System One API
}

// RegisterProxies mounts the pass-through endpoints. A body is read whole to
// find its model, within the server's body limit (MAX_BODY_MB).
func (h *Handler) RegisterProxies(e *echo.Echo) {
	for path := range passThrough {
		e.POST(path, h.PassThrough)
	}
}

// PassThrough forwards a request to the provider its body's "model" names,
// with the model rewritten to the provider's own id; everything else passes
// as it is, streamed responses included. The body is a JSON object, or a
// multipart form (an image edit's upload). A model that names no known
// provider or model, or a model of a type the endpoint does not take, is
// refused and never sent elsewhere.
func (h *Handler) PassThrough(c *echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err // the body limit's 413, or a client that went away
	}
	model, withModel := jsonModel(body)
	if mediaType, params, _ := mime.ParseMediaType(c.Request().Header.Get("Content-Type")); mediaType == "multipart/form-data" {
		model, withModel = formModel(body, params["boundary"])
	}
	if model == "" {
		return echo.NewHTTPError(http.StatusBadRequest, `the body must be a JSON object or a multipart form with a "model" naming provider/model`)
	}
	t, err := h.Registry.ResolveAs(model, passThrough[c.Path()]...)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, c.Path()+": "+err.Error())
	}
	if err := h.Registry.Forward(c.Response(), c.Request(), t, withModel(t.Model.UpstreamID)); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "the provider could not be reached").Wrap(err)
	}
	return nil
}

// jsonModel is a JSON object's "model" ("" when there is none), and the
// object with its model set to another.
func jsonModel(body []byte) (string, func(string) []byte) {
	var fields map[string]json.RawMessage
	var model string
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil {
		return "", nil
	}
	return model, func(upstream string) []byte {
		fields["model"], _ = json.Marshal(upstream)
		body, _ := json.Marshal(fields)
		return body
	}
}

// formModel is a multipart form's "model" field ("" when there is none, or
// the form does not parse), and the form with its model set to another: every
// other part, files included, as it came, under the same boundary, so the
// request's Content-Type still holds.
func formModel(body []byte, boundary string) (string, func(string) []byte) {
	type part struct {
		header textproto.MIMEHeader
		data   []byte
	}
	var parts []part
	model := -1
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		p, err := r.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return "", nil
		}
		if p.FormName() == "model" {
			model = len(parts)
		}
		parts = append(parts, part{p.Header, data})
	}
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	if model < 0 || w.SetBoundary(boundary) != nil {
		return "", nil
	}
	return string(parts[model].data), func(upstream string) []byte {
		parts[model].data = []byte(upstream)
		for _, p := range parts {
			pw, _ := w.CreatePart(p.header) // into a buffer: no errors
			_, _ = pw.Write(p.data)
		}
		_ = w.Close()
		return out.Bytes()
	}
}
