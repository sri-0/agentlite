// Package opencode is what opencode reads from a server's /.well-known/opencode
// when a user logs in with `opencode auth login <server>`: how to log in, and
// configuration that opencode fetches on every start and puts under the
// user's own (https://opencode.ai/docs/config).
package opencode

import (
	"slices"

	"agentlite/pkg/modelcatalog"
)

// Manifest is the whole document.
type Manifest struct {
	Auth   Auth   `json:"auth"`
	Config Config `json:"config"`
}

// Auth is the command whose output opencode keeps as a token, in the
// variable Env.
type Auth struct {
	Command []string `json:"command"`
	Env     string   `json:"env"`
}

// Config is the part of opencode's configuration the server gives.
type Config struct {
	Model    string              `json:"model,omitempty"` // <provider>/<model>
	Provider map[string]Provider `json:"provider"`
	MCP      map[string]MCP      `json:"mcp,omitempty"`
	Skills   Skills              `json:"skills,omitzero"`
}

// Provider is an LLM provider; NPM names the AI SDK package that speaks to it.
type Provider struct {
	NPM     string           `json:"npm"`
	Name    string           `json:"name"`
	Options Options          `json:"options"`
	Models  map[string]Model `json:"models"`
}

// Options are the provider package's options.
type Options struct {
	BaseURL string `json:"baseURL"`
}

// Model says what a model can do as far as its card says; opencode assumes
// the rest, tool calls among them.
type Model struct {
	Name       string `json:"name,omitempty"`
	ToolCall   *bool  `json:"tool_call,omitempty"`
	Reasoning  bool   `json:"reasoning,omitempty"`
	Attachment bool   `json:"attachment,omitempty"`
	Limit      Limit  `json:"limit,omitzero"`
}

// Limit is a model's context window and output, in tokens.
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// MCP is a remote MCP server.
type MCP struct {
	Type  string `json:"type"` // remote
	URL   string `json:"url"`
	OAuth bool   `json:"oauth"`
}

// Skills are where opencode finds skills: discovery indexes (the same as
// npx skills reads), by URL.
type Skills struct {
	URLs []string `json:"urls,omitempty"`
}

// ModelOf is what a model's card tells opencode.
func ModelOf(m modelcatalog.Model) Model {
	out := Model{
		Name:       m.Name,
		Reasoning:  slices.Contains(m.Capabilities, "reasoning"),
		Attachment: slices.Contains(m.Capabilities, "vision"),
	}
	if len(m.Capabilities) > 0 { // known: a model without tool calls says so
		tools := slices.Contains(m.Capabilities, "tool_calling")
		out.ToolCall = &tools
	}
	if m.ContextLength > 0 && m.MaxOutputTokens > 0 { // opencode needs both
		out.Limit = Limit{Context: m.ContextLength, Output: m.MaxOutputTokens}
	}
	return out
}
