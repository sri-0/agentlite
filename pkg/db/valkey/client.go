// Package valkey opens Valkey clients, one per logical database.
package valkey

import (
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/config"
)

// New connects to database db with the REDIS_* settings.
func New(cfg config.Redis, db int) (valkey.Client, error) {
	return valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{cfg.Addr()},
		Username:    cfg.User,
		Password:    cfg.Pass,
		SelectDB:    db,
	})
}
