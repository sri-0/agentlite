package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
)

// Run headers: the thread, run, agent and tool a request is made for. A
// tool call's requests carry them when the client's ContextHeaders is
// toolx.Headers, so a downstream system can scope, audit or authorise by
// thread.
const (
	HeaderThread     = "X-Thread-ID"
	HeaderRootThread = "X-Root-Thread-ID"
	HeaderRun        = "X-Run-ID"
	HeaderAgent      = "X-Agent-ID"
	HeaderTool       = "X-Tool-Name"
)

// sensitive are a caller's headers Forward never passes on: who the caller
// is (its credentials, and the client certificate a gateway describes) and
// which run it claims to be in. An upstream gets them from its own client
// (Config), or from rewrite when the caller's should go through.
var sensitive = []string{
	"Authorization", "Cookie", "X-Api-Key", "X-Forwarded-Client-Cert",
	HeaderThread, HeaderRootThread, HeaderRun, HeaderAgent, HeaderTool,
}

// Forward proxies r, without the caller's sensitive headers, to where
// rewrite sends it, through rt, and streams the response back as it arrives
// (event streams among them). An upstream that answers, with any status, is
// passed through; one that cannot be reached is the error, and nothing has
// been written. A client that goes away is no error.
func Forward(w http.ResponseWriter, r *http.Request, rt http.RoundTripper, rewrite func(*httputil.ProxyRequest)) error {
	var failed error
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			for _, h := range sensitive {
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
