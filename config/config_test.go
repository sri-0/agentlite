package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadDefault: the shipped configuration loads under the strict rules.
func TestLoadDefault(t *testing.T) {
	t.Setenv("CONFIG_DIR", "default")
	t.Setenv("S3_ENDPOINT", "localhost:9000")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/agentlite")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("OPEN_SLIDE_MCP_URL", "http://localhost:8090/mcp")
	cfg, err := Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Defs()[DefaultAgent]; !ok {
		t.Fatalf("no %s agent", DefaultAgent)
	}
}

// TestLoadRefuses: mistakes fail the start instead of changing behaviour.
func TestLoadRefuses(t *testing.T) {
	const assistant = "---\nid: assistant\n---\nHelp.\n"
	tests := []struct {
		name   string
		env    map[string]string
		agents map[string]string
		models string
		want   string
	}{
		{name: "pipeline cycle", want: "pipeline cycle a → b → a", agents: map[string]string{
			"a": "---\nid: a\nkind: sequential\nsubagents: [b]\n---\n",
			"b": "---\nid: b\nkind: sequential\nsubagents: [a]\n---\n",
		}},
		{name: "unknown agent key", want: "unknown field", agents: map[string]string{
			"a": "---\nid: a\nrequire_confirmaton: [write_note]\n---\n",
		}},
		{name: "unknown models key", want: "unknown field", models: "default_model: p/m\ndefault_modle: p/m\n"},
		{name: "no default agent", want: `default agent "assistant"`, agents: map[string]string{
			"assistant": "---\nid: assistant\nmode: subagent\n---\n",
		}},
		{name: "empty log level", want: "LOG_LEVEL", env: map[string]string{"LOG_LEVEL": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "agents", "assistant.md"), assistant)
			for id, body := range tt.agents {
				write(t, filepath.Join(dir, "agents", id+".md"), body)
			}
			models := tt.models
			if models == "" {
				models = "default_model: p/m\n"
			}
			write(t, filepath.Join(dir, "models.yaml"), models)
			t.Setenv("CONFIG_DIR", dir)
			t.Setenv("S3_ENDPOINT", "localhost:9000")
			t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/agentlite")
			t.Setenv("OIDC_CLIENT_SECRET", "secret")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
