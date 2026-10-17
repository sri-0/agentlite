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

	"agentlite/internal/bootstrap"
	"agentlite/internal/handler"
	"agentlite/internal/server"
)

func main() {
	_ = godotenv.Load(".env.local", ".env")
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	app, err := bootstrap.New(ctx)
	if err != nil {
		return err
	}
	defer app.Close()

	h := &handler.Handler{
		Config: app.Config, Registry: app.Registry, Runs: app.Runs, Runtime: app.Agents,
		Store: app.Store, Artifacts: app.Artifacts, Skills: app.Skills, Images: app.Images, Log: app.Log,
	}
	e := server.New(app.Config, h)
	app.Log.Info().Int("port", app.Config.Port).Int("agents", len(app.Config.Agents)).Msg("listening")
	if err := server.Run(ctx, app.Config, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
