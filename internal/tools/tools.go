// Package tools holds the function tools that do work in the world. Each is
// built with toolx.New; schemas come from struct tags. The harness tools live
// in the builtin subpackage.
package tools

import (
	"net/http"

	"agentlite/pkg/toolx"
)

// Builder makes a tool for one connector; client is that connector's HTTP
// client (pkg/transport), shared by every tool of the connector.
type Builder func(client *http.Client) (toolx.Tool, error)

// Catalog is every world tool by name. connectors.yaml groups them into
// connectors, and an agent may list them by name.
var Catalog = map[string]Builder{
	"calculate":      calculate,
	"webfetch":       webFetch,
	"write_note":     writeNote,
	"search_records": searchRecords,
}
