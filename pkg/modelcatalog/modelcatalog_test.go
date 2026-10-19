package modelcatalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestParse: both list shapes become models with their type, context window
// and capabilities.
func TestParse(t *testing.T) {
	tests := []struct {
		endpoint, body string
		want           []Model
	}{
		{"/models", `{"data":[{"id":"openai/gpt-4.1","name":"GPT-4.1","context_length":1047576,
			"supported_parameters":["tools","reasoning"],"architecture":{"input_modalities":["text","image","file"]}},
			{"id":"Qwen/Qwen3-32B","max_model_len":32768}]}`, []Model{
			{ID: "openai/gpt-4.1", Name: "GPT-4.1", ContextLength: 1047576, SupportedParameters: []string{"tools", "reasoning"},
				Architecture: Architecture{InputModalities: []string{"text", "image", "file"}}, Capabilities: []string{CapToolCalling, CapReasoning, CapVision, CapFiles}},
			{ID: "Qwen/Qwen3-32B", ContextLength: 32768, Capabilities: []string{}},
		}},
		{"/model/info", `{"data":[{"model_name":"glm-4.6","model_info":{"mode":"chat","max_input_tokens":200000,"supports_function_calling":true}},
			{"model_name":"bge-m3","model_info":{"mode":"embedding"}},{"model_name":"sora","model_info":{"mode":"video_generation"}}]}`, []Model{
			{ID: "glm-4.6", Type: TypeLLM, ContextLength: 200000, Capabilities: []string{CapToolCalling}},
			{ID: "bge-m3", Type: TypeEmbedding},
			{ID: "sora", Type: "video_generation"}, // a type no endpoint takes
		}},
	}
	for _, tt := range tests {
		got, err := Parse(tt.endpoint, []byte(tt.body))
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(tt.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tt.endpoint, diff)
		}
	}
}

// TestLoadCards: a card is found by id or alias, case aside, or by the id's
// last segment when one card alone ends with it; a broken card is reported
// and skipped, the rest still load.
func TestLoadCards(t *testing.T) {
	dir := t.TempDir()
	for file, body := range map[string]string{
		"openai/gpt-4.1-mini.md":     "---\nid: openai/gpt-4.1-mini\ncontext_length: 1047576\n---\nGood at tools.\n",
		"zai/glm-4.6.md":             "---\nid: z-ai/glm-4.6\naliases: [glm-4.6-fp8]\n---\n",
		"a/shared.md":                "---\nid: a/shared\n---\n",
		"b/shared.md":                "---\nid: b/shared\n---\n",
		"broken.md":                  "---\nname: no id\n---\n",
		"README.md":                  "# not a card\n",
		"openai/gpt-4.1-mini.md.bak": "ignored",
	} {
		path := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cards, bad := LoadCards(dir)
	if bad == nil {
		t.Error("the card without an id was not reported")
	}
	for id, want := range map[string]string{
		"openai/gpt-4.1-mini": "openai/gpt-4.1-mini", "GPT-4.1-MINI": "openai/gpt-4.1-mini", "azure/gpt-4.1-mini": "openai/gpt-4.1-mini",
		"glm-4.6-fp8": "z-ai/glm-4.6", "glm-4.6": "z-ai/glm-4.6", "shared": "", "a/shared": "a/shared",
	} {
		card, ok := cards.For(id)
		if ok != (want != "") || card.ID != want {
			t.Errorf("For(%q) = %q, %v; want %q", id, card.ID, ok, want)
		}
	}
	if card, _ := cards.For("openai/gpt-4.1-mini"); card.Notes != "Good at tools." || card.ContextLength != 1047576 {
		t.Errorf("card read as %+v", card)
	}
}

// TestMerge: a later layer replaces what it sets and leaves the rest, and no
// layer's own map is changed.
func TestMerge(t *testing.T) {
	card := Model{Name: "Card name", ContextLength: 128000, Description: "from the card", Benchmarks: map[string]float64{"mmlu": 80}}
	listed := Model{Name: "Listed name", ContextLength: 32768, Benchmarks: map[string]float64{"gpqa": 50}}
	declared := Model{ContextLength: 16384}
	got := Merge(card, listed, declared)
	want := Model{Name: "Listed name", ContextLength: 16384, Description: "from the card", Benchmarks: map[string]float64{"mmlu": 80, "gpqa": 50}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("merged (-want +got):\n%s", diff)
	}
	if len(card.Benchmarks) != 1 {
		t.Errorf("the card's own benchmarks changed: %v", card.Benchmarks)
	}
}
