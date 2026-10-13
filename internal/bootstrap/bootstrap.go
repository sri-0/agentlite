// Package bootstrap wires the application from configuration: logging,
// tracing, the provider registry, Valkey, OpenSearch, S3, sessions, memory,
// sources, artifacts, MCP servers, the agent runtime and the run manager.
// Every command starts here.
package bootstrap

import (
	"context"

	"github.com/rs/zerolog"
	valkeygo "github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/internal/agents"
	"agentlite/internal/artifacts"
	"agentlite/internal/config"
	"agentlite/internal/hitl"
	"agentlite/internal/mcp"
	"agentlite/internal/providers"
	"agentlite/internal/runs"
	"agentlite/internal/sources"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/logging"
	"agentlite/pkg/memory"
	"agentlite/pkg/s3"
	valkeysession "agentlite/pkg/session/valkey"
)

// App is the wired application.
type App struct {
	Config    *config.Config
	Log       zerolog.Logger
	Registry  *providers.Registry
	Store     *opensearch.Client
	Sessions  session.Service
	Memory    *memory.Service // nil when memory is not configured
	Sources   *sources.Recorder
	Artifacts *artifacts.Store
	Agents    *agents.Runtime
	Runs      *runs.Manager

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

	if app.Registry, err = providers.NewRegistry(cfg.Models, cfg.ModelCardsDir, app.Log); err != nil {
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
	var mem agents.Memory
	if app.Memory != nil {
		mem = app.Memory
	}
	app.Sources = sources.New(app.Store)
	if app.Artifacts, err = artifactStore(ctx, cfg, app.Store, app.Sources, app.Log); err != nil {
		return nil, err
	}
	servers, err := mcp.Servers(cfg.Connectors, app.Log)
	if err != nil {
		return nil, err
	}
	app.Agents = agents.New(cfg, agents.Deps{
		Registry: app.Registry, Sessions: app.Sessions, Memory: mem, Servers: servers,
		Sources: app.Sources, Artifacts: app.Artifacts, Log: app.Log,
	})
	app.Runs = runs.New(cfg, runsDB, hitl.NewStore(hitlDB), app.Store, app.Store, app.Sessions, app.Agents, app.Log)
	if err := app.Agents.Bind(app.Runs); err != nil { // the subagent tool dispatches through the run manager
		return nil, err
	}
	app.Runs.StartWorkers(ctx)
	app.closers = append(app.closers, app.Runs.Stop)
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

// artifactStore keeps artifact bodies in S3 when S3_ENDPOINT is set, and in
// OpenSearch otherwise.
func artifactStore(ctx context.Context, cfg *config.Config, records *opensearch.Client, recorder *sources.Recorder, log zerolog.Logger) (*artifacts.Store, error) {
	var bodies *s3.Client
	if cfg.S3.Endpoint == "" {
		log.Info().Msg("S3 not configured; artifact bodies are kept in OpenSearch")
	} else {
		var err error
		if bodies, err = s3.New(ctx, cfg.S3); err != nil {
			return nil, err
		}
	}
	return artifacts.New(records, bodies, recorder, log), nil
}

// memoryService builds long-term memory when models.yaml configures it.
func memoryService(ctx context.Context, cfg *config.Config, registry *providers.Registry, store *opensearch.Client, log zerolog.Logger) (*memory.Service, error) {
	m := cfg.Models.Memory
	if m == nil {
		return nil, nil
	}
	return memory.New(ctx, store, registry.Embedder(m.EmbeddingModel), m.Dimensions, log)
}
