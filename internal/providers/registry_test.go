package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"agentlite/internal/config"
	"agentlite/pkg/modelcatalog"
)

// TestResolve pins what routing must never do: send a model to a provider
// other than the one its id names, or to a host a model card names.
func TestResolve(t *testing.T) {
	cards := t.TempDir()
	card := "---\nid: openai/m1\nname: Card name\ncontext_length: 100\npricing: {input: 1}\n" +
		"base_url: http://card.invalid/v1\n---\nNotes.\n"
	if err := os.MkdirAll(filepath.Join(cards, "openai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cards, "openai", "m1.md"), []byte(card), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(&config.ModelsConfig{Providers: []config.Provider{
		{ID: "a", BaseURL: "http://a.invalid/v1", Models: []modelcatalog.Model{
			{ID: "openai/m1", ContextLength: 300},
		}},
		{ID: "b", BaseURL: "http://b.invalid/v1", Models: []modelcatalog.Model{{ID: "m2", BaseURL: "http://m2.invalid/v1"}}},
	}}, cards, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	r.rebuild(r.provider("a"), []modelcatalog.Model{{ID: "openai/m1", Name: "Listed", ContextLength: 200}, {ID: "m3"}})

	for _, tc := range []struct {
		id, host string
		err      error
	}{
		{"a/openai/m1", "a.invalid", nil},
		{"a/m3", "a.invalid", nil}, // discovered only
		{"b/m2", "m2.invalid", nil},
		{"b/openai/m1", "", ErrUnknownModel}, // a's model, never served by b
		{"c/openai/m1", "", ErrUnknownProvider},
		{"openai/m1", "", ErrUnknownProvider}, // no provider prefix
	} {
		got, err := r.Resolve(tc.id)
		if !errors.Is(err, tc.err) || (err == nil && got.BaseURL.Host != tc.host) {
			t.Errorf("Resolve(%q) = %v, %v; want host %q, error %v", tc.id, got.BaseURL, err, tc.host, tc.err)
		}
	}

	// card < discovery < models.yaml, field by field
	m, _ := r.Resolve("a/openai/m1")
	if m.Model.Name != "Listed" || m.Model.ContextLength != 300 || m.Model.Pricing.Input != 1 || m.Model.Notes != "Notes." {
		t.Errorf("merged model = %+v", m.Model)
	}
}

// TestNoAmbientCredentials: openai-go reads OPENAI_* variables into every
// client; none of them may reach a provider, even one without a key.
func TestNoAmbientCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-ambient")
	t.Setenv("OPENAI_ADMIN_KEY", "admin-ambient")
	t.Setenv("OPENAI_ORG_ID", "org-ambient")
	t.Setenv("OPENAI_PROJECT_ID", "proj-ambient")
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.5]}]}`))
	}))
	defer srv.Close()
	r, err := NewRegistry(&config.ModelsConfig{Providers: []config.Provider{
		{ID: "v", BaseURL: srv.URL, Models: []modelcatalog.Model{{ID: "emb", Type: "embedding"}}},
	}}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Embedder("v/emb").Embed(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project"} {
		if v := got.Get(h); v != "" {
			t.Errorf("%s: %q reached the provider", h, v)
		}
	}
}
