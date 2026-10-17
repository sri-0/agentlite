// Package wellknown serves /.well-known/: what opencode and npx skills read to
// set themselves up from this server.
package wellknown

import (
	"agentlite/internal/config"
	"agentlite/internal/providers"
	"agentlite/internal/skills"
)

// Handler serves the well-known documents.
type Handler struct {
	Config   *config.Config
	Registry *providers.Registry
	Skills   *skills.Library // nil without S3
}
