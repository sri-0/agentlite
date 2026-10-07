// Package valkey opens the Valkey client used for sessions, run logs and queues.
package valkey

import (
	"github.com/valkey-io/valkey-go"

	"agentlite/internal/config"
)

// New connects to Valkey with the REDIS_* settings.
func New(cfg config.Redis) (valkey.Client, error) {
	return valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{cfg.Addr()},
		Username:    cfg.User,
		Password:    cfg.Pass,
	})
}
