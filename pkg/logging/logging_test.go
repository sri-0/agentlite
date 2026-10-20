package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestAudit: an audit entry is written as level "audit" whatever the
// logger's level, which hides the entries below it.
func TestAudit(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.ErrorLevel)
	log.Info().Msg("hidden")
	Audit(log).Str("user_id", "u").Msg("run started")
	if got := buf.String(); !strings.Contains(got, `"level":"audit"`) || !strings.Contains(got, `"user_id":"u"`) || strings.Contains(got, "hidden") {
		t.Fatalf("got %q", got)
	}
}
