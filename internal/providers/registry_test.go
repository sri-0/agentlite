package providers

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"agentlite/internal/config"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/transport"
)

// TestResolve pins how a model id routes in the deployed, offline setup
// (LiteLLM and vLLM): to the provider its prefix names and nowhere else,
// never to a provider that merely serves a model of the same name, and never
// to a host a model card names.
func TestResolve(t *testing.T) {
	cards := t.TempDir()
	card := "---\nid: Qwen/Qwen3-32B\nname: Card name\ncontext_length: 100\npricing: {input: 1}\n" +
		"base_url: http://card.invalid/v1\n---\nNotes.\n"
	if err := os.MkdirAll(filepath.Join(cards, "Qwen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cards, "Qwen", "Qwen3-32B.md"), []byte(card), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewRegistry(&config.ModelsConfig{Providers: []config.Provider{
		{ID: "litellm", BaseURL: "http://litellm.invalid/v1", Models: []modelcatalog.Model{{ID: "gpt-oss-120b"}}},
		{ID: "vllm", BaseURL: "http://vllm.invalid/v1", Models: []modelcatalog.Model{
			{ID: "Qwen/Qwen3-32B", ContextLength: 300},
			{ID: "qwen3-embedding", Type: modelcatalog.TypeEmbedding, BaseURL: "http://vllm-embed.invalid/v1"},
		}},
		{ID: "vllm-gpu2"}, // no base_url in this environment: skipped
	}}, cards, transport.Env{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	r.rebuild(r.provider("vllm"), []modelcatalog.Model{{ID: "Qwen/Qwen3-32B", Name: "Listed", ContextLength: 200}})
	r.rebuild(r.provider("litellm"), []modelcatalog.Model{{ID: "glm-4.6"}})

	for _, tc := range []struct {
		id, host string
		err      error
	}{
		{"litellm/gpt-oss-120b", "litellm.invalid", nil},
		{"litellm/glm-4.6", "litellm.invalid", nil},         // discovered only
		{"vllm/Qwen/Qwen3-32B", "vllm.invalid", nil},        // split at the first /
		{"vllm/qwen3-embedding", "vllm-embed.invalid", nil}, // the model's own base_url
		{"litellm/Qwen/Qwen3-32B", "", ErrUnknownModel},     // vllm's model, never served by litellm
		{"vllm/gpt-oss-120b", "", ErrUnknownModel},
		{"vllm-gpu2/Qwen/Qwen3-32B", "", ErrUnknownProvider},  // skipped
		{"openrouter/Qwen/Qwen3-32B", "", ErrUnknownProvider}, // not configured
		{"Qwen/Qwen3-32B", "", ErrUnknownProvider},            // no provider prefix
	} {
		got, err := r.Resolve(tc.id)
		if !errors.Is(err, tc.err) || (err == nil && got.BaseURL.Host != tc.host) {
			t.Errorf("Resolve(%q) = %v, %v; want host %q, error %v", tc.id, got.BaseURL, err, tc.host, tc.err)
		}
	}

	// a use takes models of its types only
	if _, err := r.ResolveAs("vllm/qwen3-embedding", modelcatalog.TypeLLM); !errors.Is(err, ErrWrongType) {
		t.Errorf("an embedding model as an llm: %v", err)
	}
	if _, err := r.ResolveAs("vllm/qwen3-embedding", modelcatalog.TypeEmbedding); err != nil {
		t.Errorf("an embedding model as an embedding model: %v", err)
	}

	// card < discovery < models.yaml, field by field; the card's base_url is ignored
	m, _ := r.Resolve("vllm/Qwen/Qwen3-32B")
	if got := m.Model; got.Name != "Listed" || got.ContextLength != 300 || got.Pricing.Input != 1 || got.Notes != "Notes." {
		t.Errorf("merged model = %+v", got)
	}
	if m.Model.UpstreamID != "Qwen/Qwen3-32B" || m.Model.ID != "vllm/Qwen/Qwen3-32B" {
		t.Errorf("ids = %q, %q", m.Model.ID, m.Model.UpstreamID)
	}
}
