// Package bootstrap wires the application from configuration: logging,
// tracing, the provider registry, Valkey, OpenSearch, S3, sessions, memory,
// sources, artifacts, MCP servers, the agent runtime and the run manager.
// Every command starts here.
package bootstrap

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	valkeygo "github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"

	"agentlite/config"
	"agentlite/internal/agents"
	"agentlite/internal/artifacts"
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
	Artifacts *artifacts.Store
	Skills    *skills.Library
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

	if app.Registry, err = providers.NewRegistry(cfg.Models, cfg.ModelCardsDir, cfg.Transport, app.Log); err != nil {
		return nil, err
	}
	app.Registry.Start(ctx) // the first discovery, before the check
	if err := checkConfiguredModels(cfg.Models, app.Registry); err != nil {
		return nil, err
	}
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
	if app.Store, err = opensearch.New(cfg.OpenSearch); err != nil {
		return nil, err
	}
	app.Sessions = valkeysession.New(sessionsDB)
	var mem agents.Memory
	if m := cfg.Models.Memory; m != nil {
		app.Memory = memory.New(app.Store, app.Registry.Embedder(m.EmbeddingModel), app.Log)
		mem = app.Memory
	}
	recorder := sources.New(app.Store)
	files, err := s3.New(ctx, cfg.S3) // artifact bodies and skill files
	if err != nil {
		return nil, err
	}
	app.Artifacts = artifacts.New(app.Store, files, recorder, app.Log)
	app.Skills = skills.New(app.Store, files, app.Log)
	servers, err := mcp.Servers(cfg.Connectors, cfg.Transport, app.Log)
	if err != nil {
		return nil, err
	}
	app.Agents = agents.New(cfg, agents.Deps{
		Registry: app.Registry, Sessions: app.Sessions, Memory: mem, Servers: servers,
		Sources: recorder, Artifacts: app.Artifacts, Skills: app.Skills, Log: app.Log,
	})
	app.Runs = runs.New(cfg, runsDB, hitl.NewStore(hitlDB), app.Store, app.Store, app.Sessions, app.Agents, app.Log)
	if err := app.Agents.Bind(ctx, app.Runs); err != nil { // the subagent tool dispatches through the run manager
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
// (a run that names none uses it) and memory's embedding model. Each must be
// in the model list after the start's first discovery (declared in
// models.yaml, or listed by its provider), as the type it is used for, so a
// mistake fails the start rather than every run or memory.
func checkConfiguredModels(m *config.ModelsConfig, registry *providers.Registry) error {
	check := func(id, typ string) error {
		if _, err := registry.ResolveAs(id, typ); err != nil {
			return fmt.Errorf("models.yaml: %w (neither declared in models.yaml nor discovered)", err)
		}
		return nil
	}
	if err := check(m.DefaultModel, modelcatalog.TypeLLM); err != nil || m.Memory == nil {
		return err
	}
	return check(m.Memory.EmbeddingModel, modelcatalog.TypeEmbedding)
}
