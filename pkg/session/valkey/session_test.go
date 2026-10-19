package valkeysession

import (
	"testing"

	"github.com/valkey-io/valkey-go"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/sessiontestsuite"
)

// TestService runs adk's own session.Service suite against Valkey (database
// 15, flushed per test); skipped when Valkey is not running on localhost.
func TestService(t *testing.T) {
	db, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}, SelectDB: 15})
	if err != nil {
		t.Skip("Valkey is not running:", err)
	}
	t.Cleanup(db.Close)
	sessiontestsuite.RunServiceTests(t, sessiontestsuite.SuiteOptions{SupportsUserProvidedSessionID: true},
		func(t *testing.T) session.Service {
			if err := db.Do(t.Context(), db.B().Flushdb().Build()).Error(); err != nil {
				t.Fatal(err)
			}
			return New(db)
		})
}
