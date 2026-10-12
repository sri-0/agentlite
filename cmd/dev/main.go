// Command dev runs adk's own launcher over the same roster, sessions and
// memory as the server: `dev web` for adk's web UI with its event and trace
// views, `dev cli` for a terminal chat, `dev api` for its REST API. Runs made
// here bypass the AG-UI log and the thread store, so agentui does not see them.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"

	"agentlite/internal/bootstrap"
	"agentlite/internal/runs"
)

func main() {
	_ = godotenv.Load(".env.local", ".env")
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	app, err := bootstrap.New(ctx)
	if err != nil {
		return err
	}
	defer app.Close()
	cfg := &launcher.Config{SessionService: app.Sessions, AgentLoader: loader{app}}
	if app.Memory != nil {
		cfg.MemoryService = app.Memory.ForThread("dev")
	}
	return full.NewLauncher().Execute(ctx, cfg, os.Args[1:])
}

// loader hands adk the primary agents, built as a user-started run would.
type loader struct{ app *bootstrap.App }

func (l loader) ListAgents() []string {
	var ids []string
	for _, d := range l.app.Config.Agents {
		if d.Primary() {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

func (l loader) LoadAgent(name string) (adkagent.Agent, error) {
	return l.app.Agents.Agent(context.Background(), name)
}

func (l loader) RootAgent() adkagent.Agent {
	root, err := l.LoadAgent(runs.DefaultAgent)
	if err != nil {
		panic(err)
	}
	return root
}
