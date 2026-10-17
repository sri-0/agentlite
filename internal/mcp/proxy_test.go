package mcp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// TestForwardKeepsWhoseCallItIs: a client's sensitive headers never reach
// an exposed server, and its Authorization only one whose connector forwards
// the user's. The connector's own headers, the MCP session and the client's
// other headers, run headers among them, go through.
func TestForwardKeepsWhoseCallItIs(t *testing.T) {
	proxy := transport.Env{SensitiveHeaders: []string{"Authorization", "Cookie"}}
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Mcp-Session-Id", "s1")
	}))
	defer upstream.Close()
	client, err := transport.New(transport.Config{Headers: map[string]string{"X-Api-Key": "key"}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ := url.Parse(upstream.URL + "/mcp?key=k")

	for _, forward := range []bool{false, true} {
		srv := &Server{endpoint: endpoint, client: client, forward: forward, proxy: proxy}
		req := httptest.NewRequest(http.MethodPost, "/mcp/slides?x=1", strings.NewReader(`{}`))
		req.Header.Set("Cookie", "session=s")
		req.Header.Set(toolx.HeaderThread, "t1")
		req.Header.Set("Authorization", "Bearer user")
		req.Header.Set("Mcp-Session-Id", "s1")
		rec := httptest.NewRecorder()
		if err := srv.Forward(rec, req); err != nil {
			t.Fatal(err)
		}

		if got.URL.Path != "/mcp" || got.URL.RawQuery != "key=k" {
			t.Errorf("forward=%v: sent to %s", forward, got.URL)
		}
		if v := got.Header.Get("Cookie"); v != "" {
			t.Errorf("forward=%v: Cookie reached the server: %q", forward, v)
		}
		if v := got.Header.Get(toolx.HeaderThread); v != "t1" {
			t.Errorf("forward=%v: the server got %s %q", forward, toolx.HeaderThread, v)
		}
		if auth := got.Header.Get("Authorization"); (auth != "") != forward {
			t.Errorf("forward=%v: the server got Authorization %q", forward, auth)
		}
		if got.Header.Get("X-Api-Key") != "key" || got.Header.Get("Mcp-Session-Id") != "s1" {
			t.Errorf("forward=%v: the server got %v", forward, got.Header)
		}
		if rec.Header().Get("Mcp-Session-Id") != "s1" {
			t.Errorf("forward=%v: the client got %v", forward, rec.Header())
		}
	}

	// A server that cannot be reached is an error, with nothing written.
	upstream.Close()
	rec := httptest.NewRecorder()
	srv := &Server{endpoint: endpoint, client: client, proxy: proxy}
	err = srv.Forward(rec, httptest.NewRequest(http.MethodPost, "/mcp/slides", nil))
	if err == nil || rec.Body.Len() > 0 {
		t.Errorf("unreachable: err %v, wrote %q", err, rec.Body)
	}
}
