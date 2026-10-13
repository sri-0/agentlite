// Package config loads process configuration: environment variables plus the
// YAML and markdown files under CONFIG_DIR (models, MCP servers, agents).
package config

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/sethvargo/go-envconfig"

	"agentlite/pkg/s3"
)

// Env holds everything read from environment variables.
type Env struct {
	Port           int      `env:"PORT, default=8000"`
	Host           string   `env:"HOST, default=0.0.0.0"`
	AppName        string   `env:"APP_NAME, default=agentlite"`
	ConfigDir      string   `env:"CONFIG_DIR, default=config/default"`
	LogLevel       string   `env:"LOG_LEVEL, default=info"`
	LogJSON        bool     `env:"LOG_JSON, default=false"`
	AllowedOrigins []string `env:"ALLOWED_ORIGINS, default=http://localhost:3000"`

	Redis      Redis      `env:", prefix=REDIS_"`
	OpenSearch OpenSearch `env:", prefix=OPENSEARCH_"`
	S3         s3.Config  `env:", prefix=S3_"` // artifact bodies; no S3_ENDPOINT keeps them in OpenSearch
}

// Redis is the Valkey connection; the REDIS_ names are kept from agentic.
// Each kind of state lives in its own logical database (docs/ARCHITECTURE.md).
type Redis struct {
	Host string `env:"HOST, default=localhost"`
	Port int    `env:"PORT, default=6379"`
	User string `env:"USER"`
	Pass string `env:"PASS"`

	DBRuns     int `env:"DB_RUNS, default=1"`     // thread event logs, run records, leases, queues
	DBSessions int `env:"DB_SESSIONS, default=2"` // adk sessions: each thread's agent state and history
	DBHITL     int `env:"DB_HITL, default=3"`     // interrupt records and their decisions
}

// Addr returns host:port.
func (r Redis) Addr() string { return net.JoinHostPort(r.Host, strconv.Itoa(r.Port)) }

// OpenSearch is the OpenSearch connection.
type OpenSearch struct {
	URL      string `env:"URL, default=http://localhost:9200"`
	Username string `env:"USERNAME"`
	Password string `env:"PASSWORD"`
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
	var err error
	if cfg.Models, err = LoadModels(cfg.ConfigDir); err != nil {
		return nil, err
	}
	if cfg.Connectors, err = LoadConnectors(cfg.ConfigDir); err != nil {
		return nil, err
	}
	if cfg.Agents, err = LoadAgents(cfg.ConfigDir); err != nil {
		return nil, err
	}
	return cfg, nil
}
