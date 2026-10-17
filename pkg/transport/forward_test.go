package transport

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

// TestForwardDropsSensitiveHeaders: none of a caller's sensitive headers
// reach the upstream, rewrite may set the upstream's own, and the caller's
// other headers go through.
func TestForwardDropsSensitiveHeaders(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for _, h := range sensitive {
		req.Header.Set(h, "the caller's")
	}
	req.Header.Set("Content-Type", "application/json")
	err := Forward(httptest.NewRecorder(), req, http.DefaultTransport, func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.Header.Set("Authorization", "Bearer upstream")
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range sensitive {
		want := ""
		if h == "Authorization" {
			want = "Bearer upstream"
		}
		if v := got.Get(h); v != want {
			t.Errorf("%s: got %q, want %q", h, v, want)
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type did not go through: %v", got)
	}
}
