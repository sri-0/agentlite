package providers

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"agentlite/pkg/transport"
)

// Forward sends a pass-through request, with body as its body, to the
// target's base URL (transport.Forward). The incoming /v1 prefix is dropped:
// base URLs end with it.
func (r *Registry) Forward(w http.ResponseWriter, req *http.Request, t Target, body []byte) error {
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.Header.Del("Content-Length")
	return transport.Forward(w, req, t.Client.HTTP.Transport, func(pr *httputil.ProxyRequest) {
		pr.Out.URL = &url.URL{
			Scheme: t.BaseURL.Scheme, Host: t.BaseURL.Host,
			Path: t.BaseURL.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1"), RawQuery: pr.In.URL.RawQuery,
		}
		pr.Out.Host = t.BaseURL.Host
		// The caller's Authorization is the internal user's JWT; it must
		// never reach a provider, which gets its own key instead.
		pr.Out.Header.Del("Authorization")
		if auth := t.Client.Authorization(); auth != "" {
			pr.Out.Header.Set("Authorization", auth)
		}
		pr.Out.Header.Set("Accept-Encoding", "identity")
	})
}
