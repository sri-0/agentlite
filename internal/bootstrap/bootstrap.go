// Package bootstrap wires the application from configuration: logging,
// tracing, the provider registry, Valkey, OpenSearch, sessions, memory, MCP
// servers, the engine and the run service. Every command starts here.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	valkeygo "github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/internal/agent"
	"agentlite/internal/config"
	"agentlite/internal/engine"
	"agentlite/internal/hitl"
	"agentlite/internal/mcp"
	"agentlite/internal/proxy"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/logging"
	"agentlite/pkg/memory"
	valkeysession "agentlite/pkg/session/valkey"
)

// App is the wired application.
type App struct {
	Config   *config.Config
	Log      zerolog.Logger
	Registry *proxy.Registry
	Store    *opensearch.Client
	Sessions session.Service
	Memory   *memory.Service // nil when memory is not configured
	Engine   *engine.Engine
	Service  *agent.Service

	closers []func()
}

// New wires everything and starts the worker loops; Close stops them.
func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, err
	}
	app := &App{Config: cfg, Log: logging.New(cfg.LogLevel, cfg.LogJSON)}
	stopTracing, err := tracing(ctx, cfg)
	if err != nil {
		return nil, err
	}
	app.closers = append(app.closers, stopTracing)

	if app.Registry, err = proxy.NewRegistry(cfg.Models, app.Log); err != nil {
		return nil, err
	}
	app.Registry.Start(ctx)
	// One client per logical database: runs, sessions and interrupts are kept apart.
	runsDB, err := app.valkey(cfg.Redis.DBRuns)
	if err != nil {
		return nil, err
	}
	sessionsDB, err := app.valkey(cfg.Redis.DBSessions)
	if err != nil {
		return nil, err
	}
	hitlDB, err := app.valkey(cfg.Redis.DBHITL)
	if err != nil {
		return nil, err
	}
	if app.Store, err = opensearch.New(ctx, cfg.OpenSearch, app.Log); err != nil {
		return nil, err
	}
	app.Sessions = valkeysession.New(sessionsDB)
	if app.Memory, err = memoryService(ctx, cfg, app.Registry, app.Store, app.Log); err != nil {
		return nil, err
	}
	var mem engine.Memory
	if app.Memory != nil {
		mem = app.Memory
	}
	app.Engine = engine.New(cfg, app.Registry, app.Sessions, mem, mcp.Servers(cfg.MCP, app.Log), app.Log)
	app.Service = agent.NewService(cfg, runsDB, hitl.NewStore(hitlDB), app.Store, app.Store, app.Sessions, app.Engine, app.Log)
	if err := app.Engine.Bind(app.Service); err != nil { // the task tool dispatches through the service
		return nil, err
	}
	app.Service.StartWorkers(ctx)
	app.closers = append(app.closers, app.Service.Stop)
	return app, nil
}

// valkey opens a client on one logical database and closes it with the app.
func (a *App) valkey(db int) (valkeygo.Client, error) {
	c, err := valkey.New(a.Config.Redis, db)
	if err != nil {
		return nil, err
	}
	a.closers = append(a.closers, c.Close)
	return c, nil
}

// Close stops the workers and releases connections, newest first.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

// memoryService builds long-term memory when models.yaml configures it.
func memoryService(ctx context.Context, cfg *config.Config, registry *proxy.Registry, store *opensearch.Client, log zerolog.Logger) (*memory.Service, error) {
	m := cfg.Models.Memory
	if m == nil {
		return nil, nil
	}
	client, _, err := registry.Model(m.EmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	return memory.New(ctx, store, client, m.EmbeddingModel, m.Dimensions, log)
}
