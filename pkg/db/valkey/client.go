// Package valkey opens Valkey clients, one per logical database.
package valkey

import (
	"net"
	"strconv"

	"github.com/valkey-io/valkey-go"
)

// Config is the Valkey connection. Each kind of state lives in its own
// logical database (docs/ARCHITECTURE.md).
type Config struct {
	Host string `env:"HOST, default=localhost"`
	Port int    `env:"PORT, default=6379"`
	User string `env:"USER"`
	Pass string `env:"PASS"`

	DBRuns     int `env:"DB_RUNS, default=1"`     // thread event logs, run records, leases, queues
	DBSessions int `env:"DB_SESSIONS, default=2"` // adk sessions: each thread's agent state and history
	DBHITL     int `env:"DB_HITL, default=3"`     // interrupt records and their decisions
}

// New connects to database db.
func New(cfg Config, db int) (valkey.Client, error) {
	return valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))},
		Username:    cfg.User,
		Password:    cfg.Pass,
		SelectDB:    db,
	})
}
