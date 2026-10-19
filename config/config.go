// Package config loads process configuration: environment variables plus the
// YAML and markdown files under CONFIG_DIR (models, MCP servers, agents).
package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/goccy/go-yaml"
	"github.com/rs/zerolog"
	"github.com/sethvargo/go-envconfig"

	"agentlite/internal/auth"
	"agentlite/internal/tokens"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/s3"
	"agentlite/pkg/transport"
)

// Env holds everything read from environment variables.
type Env struct {
	Port           int           `env:"PORT, default=8000"`
	Host           string        `env:"HOST, default=0.0.0.0"`
	AppName        string        `env:"APP_NAME, default=agentlite"`
	ConfigDir      string        `env:"CONFIG_DIR, default=config/default"`
	LogLevel       zerolog.Level `env:"LOG_LEVEL, default=info"`
	LogJSON        bool          `env:"LOG_JSON, default=false"`
	AllowedOrigins []string      `env:"ALLOWED_ORIGINS, default=http://localhost:3000"` // CORS; * allows any
	WS             WS            `env:", prefix=WS_"`
	// AppURL is this server as clients reach it (through any gateway in
	// front): the base of the URLs /.well-known/opencode hands out.
	AppURL string `env:"APP_URL, default=http://localhost:8000"`
	// MaxBodyMB is the largest request body the server accepts; the
	// pass-through reads one whole to find its model.
	MaxBodyMB int64 `env:"MAX_BODY_MB, default=32"`

	// OIDC is the access tokens' issuer: every request but /health needs one
	// (docs/AUTH.md).
	OIDC auth.Config `env:", prefix=OIDC_"`
	// Tokens is the server's own client at the issuer, which exchanges a
	// user's token for the thread token their runs use.
	Tokens     tokens.Config     `env:", prefix=OIDC_"`
	Redis      valkey.Config     `env:", prefix=REDIS_"` // the REDIS_ names are kept from agentic
	OpenSearch opensearch.Config `env:", prefix=OPENSEARCH_"`
	// S3 keeps artifact bodies and skill files; OpenSearch keeps their records.
	S3 s3.Config `env:", prefix=S3_"`
	// Transport is how requests to upstreams are made, e.g. the headers a
	// forward drops.
	Transport transport.Env `env:", prefix=TRANSPORT_"`
	// ModelCardsDir is a checkout of the model cards (one Markdown file per
	// model); empty means no cards.
	ModelCardsDir string `env:"MODEL_CARDS_DIR"`
}

// WS is the agent chat socket's configuration (WS_*).
type WS struct {
	// AllowedOrigins are the browser pages that may open the socket, as
	// coder/websocket's OriginPatterns; * allows any. Clients that are not
	// browsers send no origin.
	AllowedOrigins []string `env:"ALLOWED_ORIGINS, default=http://localhost:3000"`
}

// Config is the full configuration: environment plus the files in ConfigDir.
type Config struct {
	Env
	Models     *ModelsConfig
	Connectors []Connector
	Agents     []AgentDef
}

// Defs indexes the agents by id.
func (c *Config) Defs() map[string]AgentDef {
	defs := make(map[string]AgentDef, len(c.Agents))
	for _, d := range c.Agents {
		defs[d.ID] = d
	}
	return defs
}

// Load reads the environment and every configuration file.
func Load(ctx context.Context) (*Config, error) {
	cfg := &Config{}
	if err := envconfig.Process(ctx, &cfg.Env); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	if err := cfg.Env.check(); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	var err error
	if cfg.Models, err = loadModels(cfg.ConfigDir); err != nil {
		return nil, err
	}
	cc, err := loadConnectors(cfg.ConfigDir)
	if err != nil {
		return nil, err
	}
	cfg.Connectors = cc.Connectors
	if cfg.Agents, err = loadAgents(cfg.ConfigDir); err != nil {
		return nil, err
	}
	return cfg, nil
}

// check refuses settings the server would misread: a log level that hides
// every line, an empty origin list, a body limit that refuses everything.
func (e Env) check() error {
	u, err := url.Parse(e.AppURL)
	switch {
	case e.LogLevel == zerolog.NoLevel:
		return errors.New("LOG_LEVEL is empty; set debug, info, warn or error")
	case len(e.AllowedOrigins) == 0 || len(e.WS.AllowedOrigins) == 0:
		return errors.New("ALLOWED_ORIGINS and WS_ALLOWED_ORIGINS need at least one origin, or *")
	case e.MaxBodyMB <= 0:
		return errors.New("MAX_BODY_MB must be positive")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return fmt.Errorf("APP_URL %q is not an absolute http(s) URL", e.AppURL)
	}
	return nil
}

// decode reads YAML strictly: an unknown key, such as a misspelt
// require_confirmation, is an error rather than a setting silently dropped.
func decode(data []byte, v any) error {
	return yaml.UnmarshalWithOptions(data, v, yaml.DisallowUnknownField())
}
