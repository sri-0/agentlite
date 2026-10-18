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
	"agentlite/internal/images"
	"agentlite/internal/mcp"
	"agentlite/internal/providers"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/internal/sources"
	"agentlite/internal/voice"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/logging"
	"agentlite/pkg/memory"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/s3"
	valkeysession "agentlite/pkg/session/valkey"
	"agentlite/pkg/toolx"
)

// App is the wired application.
type App struct {
	Config    *config.Config
	Log       zerolog.Logger
	Registry  *providers.Registry
	Store     *opensearch.Client
	Sessions  session.Service
	Memory    *memory.Service    // nil when memory is not configured
	Voice     *voice.Transcriber // nil when the voice prompt is not configured
	Images    *images.Studio     // nil when image generation is not configured
	Artifacts *artifacts.Store
	Skills    *skills.Library
	Agents    *agents.Runtime
	Runs      *runs.Manager

	closers []func()
	cancel  context.CancelFunc // stops the worker loops and discovery
}

// New wires everything and starts the worker loops; Close stops them. When
// it fails partway, what it started is closed.
func New(ctx context.Context) (_ *App, err error) {
	ctx, cancel := context.WithCancel(ctx)
	app := &App{cancel: cancel}
	defer func() {
		if err != nil {
			app.Close()
		}
	}()
	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, err
	}
	app.Config, app.Log = cfg, logging.New(cfg.LogLevel, cfg.LogJSON)
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
	if v := cfg.Models.Voice; v != nil {
		app.Voice = voice.New(app.Registry, v.Model)
	}
	recorder := sources.New(app.Store)
	files, err := s3.New(ctx, cfg.S3) // artifact bodies and skill files
	if err != nil {
		return nil, err
	}
	app.Artifacts = artifacts.New(app.Store, files, recorder, app.Log)
	app.Skills = skills.New(app.Store, files, app.Log)
	var tools []toolx.Tool // harness tools of feature packages
	if m := cfg.Models.Images; m != nil {
		app.Images = images.New(app.Registry, app.Artifacts, app.Store, m.Model)
		img, err := app.Images.Tool()
		if err != nil {
			return nil, err
		}
		tools = append(tools, img)
	}
	servers, err := mcp.Servers(cfg.Connectors, cfg.Transport, app.Log)
	if err != nil {
		return nil, err
	}
	app.Agents = agents.New(cfg, agents.Deps{
		Registry: app.Registry, Sessions: app.Sessions, Memory: mem, Servers: servers,
		Sources: recorder, Artifacts: app.Artifacts, Skills: app.Skills, Tools: tools, Log: app.Log,
	})
	app.Runs = runs.New(cfg, runs.Deps{
		DB: runsDB, Interrupts: hitl.NewStore(hitlDB), History: app.Store, Sessions: app.Sessions,
		Runtime: app.Agents, Log: app.Log,
	})
	if err := app.Agents.Bind(ctx, app.Runs); err != nil { // the subagent tool dispatches through the run manager
		return nil, err
	}
	app.closers = append(app.closers, app.Agents.Wait, app.Runs.Stop) // the workers stop first
	if err := app.Runs.StartWorkers(ctx); err != nil {
		return nil, err
	}
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

// Close stops the worker loops and discovery, waits for the runs and the
// memory writes in flight, and releases connections, newest first.
func (a *App) Close() {
	a.cancel()
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
}

// checkConfiguredModels checks the models models.yaml names: default_model
// (a run that names none uses it), memory's embedding model, the voice
// prompt's transcription model and the default image model. Each must be in
// the model list after the start's first discovery (declared in models.yaml,
// or listed by its provider), as the type it is used for, so a mistake fails
// the start rather than every run, memory write, recording or image.
func checkConfiguredModels(m *config.ModelsConfig, registry *providers.Registry) error {
	check := func(id, typ string) error {
		if _, err := registry.ResolveAs(id, typ); err != nil {
			return fmt.Errorf("models.yaml: %w (neither declared in models.yaml nor discovered)", err)
		}
		return nil
	}
	if err := check(m.DefaultModel, modelcatalog.TypeLLM); err != nil {
		return err
	}
	if m.Memory != nil {
		if err := check(m.Memory.EmbeddingModel, modelcatalog.TypeEmbedding); err != nil {
			return err
		}
	}
	if m.Voice != nil {
		if err := check(m.Voice.Model, modelcatalog.TypeTranscription); err != nil {
			return err
		}
	}
	if m.Images != nil {
		return check(m.Images.Model, modelcatalog.TypeImage)
	}
	return nil
}
