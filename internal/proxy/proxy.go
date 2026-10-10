package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/rs/zerolog"
)

// NewReverseProxy forwards a request to the provider byte for byte, replacing
// the Authorization header and letting streamed responses flush as they arrive.
// The incoming /v1 prefix is dropped because base_url already ends with it.
func NewReverseProxy(c *Client, log zerolog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{
				Scheme: c.BaseURL.Scheme, Host: c.BaseURL.Host,
				Path: c.BaseURL.Path + strings.TrimPrefix(pr.In.URL.Path, "/v1"), RawQuery: pr.In.URL.RawQuery,
			}
			pr.Out.Host = c.BaseURL.Host
			pr.Out.Header.Del("Authorization")
			if auth := c.Authorization(); auth != "" {
				pr.Out.Header.Set("Authorization", auth)
			}
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		Transport:     c.HTTP.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Error().Err(err).Str("provider", c.Config.ID).Str("path", r.URL.Path).Msg("upstream error")
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
}
