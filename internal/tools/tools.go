// Package tools holds the function tools that do work in the world. Each is
// built with toolx.New; schemas come from struct tags. The harness tools live
// in the builtin subpackage.
package tools

import (
	"agentlite/config"
	"agentlite/pkg/toolx"
)

// Builder makes a tool for one connector: the system it belongs to, with the
// HTTP client every tool of that system shares.
type Builder func(c toolx.Connector) (toolx.Tool, error)

// Catalog is every world tool by name. connectors.yaml groups them into
// connectors, and an agent may list them by name.
var Catalog = map[string]Builder{
	"calculate":      calculate,
	"webfetch":       webFetch,
	"write_note":     writeNote,
	"search_records": searchRecords,
}

// Own are the server's own connectors, by their one tool: tools on its own
// services (GenerateImage, SearchFiles) rather than on a connector's HTTP
// client. The runtime offers each one it builds after connectors.yaml's, so a
// thread switches it off like any connector.
var Own = map[string]config.Connector{
	generateImageTool: {ID: "images", Name: "Image generation", Description: "Make and edit images with the image model.",
		Icon: "image", Type: config.ConnectorTools, Tools: []string{generateImageTool}},
	searchFilesTool: {ID: "files", Name: "Your files", Description: "Search the files uploaded to the chat's workspace.",
		Icon: "files", Type: config.ConnectorTools, Tools: []string{searchFilesTool}},
}
