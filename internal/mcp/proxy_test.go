package mcp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agentlite/pkg/transport"
)

// TestForwardKeepsWhoseCallItIs: an exposed server learns which run a call
// is in from agentlite alone, so a client cannot set the run headers, nor
// send its Authorization to a connector that does not forward the user's.
// The connector's own headers and the MCP session still go through.
var runHeaders = []string{
	transport.HeaderThread, transport.HeaderRootThread, transport.HeaderRun, transport.HeaderAgent, transport.HeaderTool,
}

func TestForwardKeepsWhoseCallItIs(t *testing.T) {
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
		srv := &Server{endpoint: endpoint, client: client, forward: forward}
		req := httptest.NewRequest(http.MethodPost, "/mcp/slides?x=1", strings.NewReader(`{}`))
		for _, h := range runHeaders {
			req.Header.Set(h, "spoofed")
		}
		req.Header.Set("Authorization", "Bearer user")
		req.Header.Set("Mcp-Session-Id", "s1")
		rec := httptest.NewRecorder()
		if err := srv.Forward(rec, req); err != nil {
			t.Fatal(err)
		}

		if got.URL.Path != "/mcp" || got.URL.RawQuery != "key=k" {
			t.Errorf("forward=%v: sent to %s", forward, got.URL)
		}
		for _, h := range runHeaders {
			if v := got.Header.Get(h); v != "" {
				t.Errorf("forward=%v: %s reached the server: %q", forward, h, v)
			}
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
	srv := &Server{endpoint: endpoint, client: client}
	err = srv.Forward(rec, httptest.NewRequest(http.MethodPost, "/mcp/slides", nil))
	if err == nil || rec.Body.Len() > 0 {
		t.Errorf("unreachable: err %v, wrote %q", err, rec.Body)
	}
}
