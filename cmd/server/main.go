// Command server runs the agentlite API: proxies, agents, threads and the
// agent chat WebSocket.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	adkmemory "google.golang.org/adk/v2/memory"

	"agentlite/agents"
	"agentlite/internal/agent"
	"agentlite/internal/config"
	"agentlite/internal/handler"
	"agentlite/internal/mcp"
	"agentlite/internal/proxy"
	"agentlite/internal/server"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/db/valkey"
	"agentlite/pkg/logging"
	"agentlite/pkg/memory"
)

func main() {
	_ = godotenv.Load(".env.local", ".env")
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// memoryService builds long-term memory when models.yaml configures it. The
// nil interface keeps memory tools out of agents that cannot use them.
func memoryService(ctx context.Context, cfg *config.Config, registry *proxy.Registry, store *opensearch.Client, log zerolog.Logger) (adkmemory.Service, error) {
	m := cfg.Models.Memory
	if m == nil {
		return nil, nil
	}
	client, _, err := registry.Model("", m.EmbeddingModel)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	return memory.New(ctx, store, client, m.EmbeddingModel, m.Dimensions, log)
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, cfg.LogJSON)

	registry, err := proxy.NewRegistry(cfg.Models, log)
	if err != nil {
		return err
	}
	registry.Start(ctx)

	db, err := valkey.New(cfg.Redis)
	if err != nil {
		return err
	}
	defer db.Close()
	store, err := opensearch.New(ctx, cfg.OpenSearch, log)
	if err != nil {
		return err
	}

	sessions := agents.NewSessionService(db)
	mem, err := memoryService(ctx, cfg, registry, store, log)
	if err != nil {
		return err
	}
	engine := agents.New(cfg, registry, sessions, mem, mcp.Toolsets(cfg.MCP, log), log)
	svc := agent.NewService(db, agent.NewValkeyLog(db), store, store, sessions, engine, cfg.Agents, cfg.AppName, log)
	if err := engine.Bind(svc); err != nil { // the task tool dispatches through the service
		return err
	}
	svc.StartWorkers(ctx)

	h := &handler.Handler{Config: cfg, Registry: registry, Service: svc, Store: store, Log: log}
	e := server.New(cfg, h, log)
	log.Info().Int("port", cfg.Port).Int("agents", len(cfg.Agents)).Msg("listening")
	if err := server.Run(ctx, cfg, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
