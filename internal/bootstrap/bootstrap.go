// Package bootstrap wires the application from configuration: logging,
// tracing, the provider registry, Valkey, OpenSearch, S3, sessions, memory,
// sources, artifacts, MCP servers, the agent runtime and the run manager.
// Every command starts here.
package bootstrap

import (
	"context"
	"errors"
	"fmt"

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
	"agentlite/internal/skills"
	"agentlite/internal/sources"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/logging"
	"agentlite/pkg/memory"
	"agentlite/pkg/modelcatalog"
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
	Skills    *skills.Library // nil without S3
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
	if err := checkConfiguredModels(cfg.Models, app.Registry); err != nil {
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
	var files *s3.Client // artifact bodies and skill files
	if cfg.S3.Endpoint == "" {
		app.Log.Info().Msg("S3 not configured: artifact bodies are kept in OpenSearch, and there is no skills library")
	} else if files, err = s3.New(ctx, cfg.S3); err != nil {
		return nil, err
	}
	app.Artifacts = artifacts.New(app.Store, files, app.Sources, app.Log)
	if files != nil {
		if app.Skills, err = skills.New(ctx, app.Store, files, app.Log); err != nil {
			return nil, err
		}
	}
	servers, err := mcp.Servers(cfg.Connectors, app.Log)
	if err != nil {
		return nil, err
	}
	app.Agents = agents.New(cfg, agents.Deps{
		Registry: app.Registry, Sessions: app.Sessions, Memory: mem, Servers: servers,
		Sources: app.Sources, Artifacts: app.Artifacts, Skills: app.Skills, Log: app.Log,
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

// checkConfiguredModels checks the models models.yaml names: default_model
// (a run that names none uses it) and memory's embedding model. Each must
// resolve as the type it is used for, so a mistake fails the start rather
// than every run or memory; one not listed yet may still come from
// discovery.
func checkConfiguredModels(m *config.ModelsConfig, registry *providers.Registry) error {
	check := func(id, typ string) error {
		if _, err := registry.ResolveAs(id, typ); err != nil && !errors.Is(err, providers.ErrUnknownModel) {
			return fmt.Errorf("models.yaml: %w", err)
		}
		return nil
	}
	if err := check(m.DefaultModel, modelcatalog.TypeLLM); err != nil || m.Memory == nil {
		return err
	}
	return check(m.Memory.EmbeddingModel, modelcatalog.TypeEmbedding)
}

// memoryService builds long-term memory when models.yaml configures it.
func memoryService(
	ctx context.Context, cfg *config.Config, registry *providers.Registry, store *opensearch.Client, log zerolog.Logger,
) (*memory.Service, error) {
	m := cfg.Models.Memory
	if m == nil {
		return nil, nil
	}
	return memory.New(ctx, store, registry.Embedder(m.EmbeddingModel), m.Dimensions, log)
}
