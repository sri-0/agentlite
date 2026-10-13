package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// maxPassThroughBody bounds a pass-through request, which is read whole to
// find its model.
const maxPassThroughBody = 32 << 20

// Passthrough forwards an OpenAI-style request to the provider its body's
// "model" names, with the model rewritten to the provider's own id;
// everything else passes as it is, streamed responses included. A model that
// names no known provider or model is refused, never sent elsewhere.
func (r *Registry) Passthrough(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxPassThroughBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var fields map[string]json.RawMessage
	var model string
	if err := json.Unmarshal(body, &fields); err != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
		writeError(w, http.StatusBadRequest, `the body must be a JSON object with a "model" of the form <provider>/<model>`)
		return
	}
	t, err := r.Resolve(model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fields["model"], _ = json.Marshal(t.Model.UpstreamID)
	body, _ = json.Marshal(fields)
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.Header.Del("Content-Length")
	reverseProxy(t, r).ServeHTTP(w, req)
}

// reverseProxy sends the request to the target's base URL byte for byte,
// replacing the Authorization header and letting streamed responses flush
// as they arrive. The incoming /v1 prefix is dropped: base URLs end with it.
func reverseProxy(t Target, r *Registry) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme: t.BaseURL.Scheme, Host: t.BaseURL.Host,
				Path: t.BaseURL.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1"), RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = t.BaseURL.Host
			pr.Out.Header.Del("Authorization")
			if auth := t.Client.Authorization(); auth != "" {
				pr.Out.Header.Set("Authorization", auth)
			}
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		Transport:     t.Client.HTTP.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			r.log.Error().Err(err).Str("provider", t.Client.Config.ID).Str("path", req.URL.Path).Msg("upstream error")
			writeError(w, http.StatusBadGateway, "upstream error")
		},
	}
}

// writeError answers in the OpenAI error shape, which pass-through clients
// already understand.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "invalid_request_error"}})
}
