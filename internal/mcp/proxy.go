package mcp

import (
	"net/http"
	"net/http/httputil"

	"agentlite/pkg/transport"
)

// Exposed reports whether the connector publishes the server to clients
// (mcp.expose).
func (s *Server) Exposed() bool { return s.endpoint != nil }

// Forward passes a client's MCP request (streamable HTTP: JSON-RPC posts and
// event streams) to an exposed server with the connector's client, so it
// carries the connector's headers and certificate. Like every forward it
// drops the client's credentials and run headers (transport.Forward): the
// client is in no run, and only agentlite says which run a call is in. The
// client's Authorization, the user's, goes on when the connector forwards
// the user's identity (http.forward).
func (s *Server) Forward(w http.ResponseWriter, r *http.Request) error {
	return transport.Forward(w, r, s.client.Transport, func(pr *httputil.ProxyRequest) {
		u := *s.endpoint
		pr.Out.URL, pr.Out.Host = &u, ""
		if auth := pr.In.Header.Get("Authorization"); s.forward && auth != "" {
			pr.Out.Header.Set("Authorization", auth)
		}
	})
}
