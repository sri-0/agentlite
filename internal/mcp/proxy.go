package mcp

import (
	"net/http"
	"net/http/httputil"

	"agentlite/pkg/toolx"
	"agentlite/pkg/transport"
)

// identity are the headers through which agentlite tells a server whose call
// it is (toolx.Headers, with the user's Authorization).
var identity = []string{
	toolx.HeaderThread, toolx.HeaderRootThread, toolx.HeaderRun, toolx.HeaderAgent, toolx.HeaderTool,
}

// Exposed reports whether the connector publishes the server to clients
// (mcp.expose).
func (s *Server) Exposed() bool { return s.endpoint != nil }

// Forward passes a client's MCP request (streamable HTTP: JSON-RPC posts and
// event streams) to an exposed server with the connector's client, so it
// carries the connector's headers and certificate (transport.Forward). Only
// agentlite tells the server whose call it is: the client's identity headers
// are dropped, and its Authorization too unless the connector forwards the
// user's (http.forward).
func (s *Server) Forward(w http.ResponseWriter, r *http.Request) error {
	return transport.Forward(w, r, s.client.Transport, func(pr *httputil.ProxyRequest) {
		u := *s.endpoint
		pr.Out.URL, pr.Out.Host = &u, ""
		for _, h := range identity {
			pr.Out.Header.Del(h)
		}
		if !s.forward {
			pr.Out.Header.Del("Authorization")
		}
	})
}
