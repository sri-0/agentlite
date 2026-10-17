package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
)

// Forward proxies r, without the caller's SensitiveHeaders, to where
// rewrite sends it, through rt, and streams the response back as it arrives
// (event streams among them). An upstream that answers, with any status, is
// passed through; one that cannot be reached is the error, and nothing has
// been written. A client that goes away is no error.
func (e Env) Forward(w http.ResponseWriter, r *http.Request, rt http.RoundTripper, rewrite func(*httputil.ProxyRequest)) error {
	var failed error
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			for _, h := range e.SensitiveHeaders {
				pr.Out.Header.Del(h)
			}
			rewrite(pr)
		},
		Transport:     rt,
		FlushInterval: -1,
		ErrorHandler:  func(_ http.ResponseWriter, _ *http.Request, err error) { failed = err },
	}
	proxy.ServeHTTP(w, r)
	if errors.Is(failed, context.Canceled) {
		return nil
	}
	return failed
}
