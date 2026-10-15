package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Forward sends a pass-through request, with body as its body, to the
// target's base URL, and streams the response back as it arrives. An upstream
// that answers, with any status, is passed through; one that cannot be
// reached is the error, and nothing has been written. The incoming /v1
// prefix is dropped: base URLs end with it.
func (r *Registry) Forward(w http.ResponseWriter, req *http.Request, t Target, body []byte) error {
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	req.Header.Del("Content-Length")
	var failed error
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
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
		},
		Transport:     t.Client.HTTP.Transport,
		FlushInterval: -1,
		ErrorHandler:  func(_ http.ResponseWriter, _ *http.Request, err error) { failed = err },
	}
	proxy.ServeHTTP(w, req)
	if errors.Is(failed, context.Canceled) { // the client went away
		return nil
	}
	return failed
}
