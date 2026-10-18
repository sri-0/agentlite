package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"agentlite/config"
	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/transport"
)

// fakeServer records each embeddings request's JSON and answers with a
// vector per input whose first value is the input's place in the request,
// returned in reverse order to check that vectors are put back in order.
type fakeServer struct {
	mu       sync.Mutex
	requests []map[string]any
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	n := 1
	if in, ok := body["input"].([]any); ok {
		n = len(in)
	}
	data := make([]map[string]any, n)
	for i := range n {
		v := make([]float64, 1024) // longer than asked: cut to the configured size
		v[0] = float64(i)
		data[n-1-i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": body["model"]})
}

func newEmbedder(t *testing.T, format string) (*Embedder, *fakeServer) {
	t.Helper()
	fake := &fakeServer{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	registry, err := providers.NewRegistry(&config.ModelsConfig{Providers: []config.Provider{{
		ID: "emb", BaseURL: srv.URL + "/v1", Models: []modelcatalog.Model{{ID: "google/embeddinggemma-2", Type: modelcatalog.TypeEmbedding}},
	}}}, t.TempDir(), transport.Env{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return NewEmbedder(registry, "emb/google/embeddinggemma-2", format, 768), fake
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestEmbedDeepInfra: batches of up to batchSize inputs, each a text or a
// list with one content part, with the task and the dimensions; vectors come
// back in input order, cut to the configured size.
func TestEmbedDeepInfra(t *testing.T) {
	e, fake := newEmbedder(t, "deepinfra")
	inputs := make([]input, batchSize)
	for i := range inputs {
		inputs[i] = input{text: "t"}
	}
	inputs = append(inputs,
		input{data: []byte("img"), mediaType: "image/png"},
		input{data: []byte("wav"), mediaType: "audio/wav"},
		input{data: []byte("mp3"), mediaType: "audio/mpeg"},
		input{data: []byte("mp4"), mediaType: "video/mp4"},
	)
	vectors, err := e.embed(context.Background(), taskDocument, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(inputs) || len(vectors[0]) != 768 || vectors[1][0] != 1 || vectors[batchSize+1][0] != 1 {
		t.Fatalf("got %d vectors of %d; second's first value %v", len(vectors), len(vectors[0]), vectors[1][0])
	}
	if len(fake.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(fake.requests))
	}
	first, second := fake.requests[0], fake.requests[1]
	if first["model"] != "google/embeddinggemma-2" || first["task"] != "document" || first["dimensions"] != 768.0 ||
		first["encoding_format"] != "float" || len(first["input"].([]any)) != batchSize || first["input"].([]any)[0] != "t" {
		t.Errorf("first request %s", asJSON(first))
	}
	want := `[[{"image_url":{"url":"data:image/png;base64,aW1n"},"type":"image_url"}],` +
		`[{"input_audio":{"data":"d2F2","format":"wav"},"type":"input_audio"}],` +
		`[{"input_audio":{"data":"bXAz","format":"mp3"},"type":"input_audio"}],` +
		`[{"type":"video_url","video_url":{"url":"data:video/mp4;base64,bXA0"}}]]`
	if got := asJSON(second["input"]); got != want {
		t.Errorf("media inputs\n got %s\nwant %s", got, want)
	}
}

// TestEmbedVLLM: one chat-style request per input, EmbeddingGemma's task
// prefix written into a text, media as content parts.
func TestEmbedVLLM(t *testing.T) {
	e, fake := newEmbedder(t, "vllm")
	_, err := e.embed(context.Background(), taskQuery, []input{{text: "red bike"}, {data: []byte("img"), mediaType: "image/png"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(fake.requests))
	}
	text, image := fake.requests[0], fake.requests[1]
	if got, want := asJSON(text["messages"]), `[{"content":[{"text":"task: search result | query: red bike","type":"text"}],"role":"user"}]`; got != want {
		t.Errorf("text messages\n got %s\nwant %s", got, want)
	}
	if got, want := asJSON(image["messages"]), `[{"content":[{"image_url":{"url":"data:image/png;base64,aW1n"},"type":"image_url"}],"role":"user"}]`; got != want {
		t.Errorf("image messages\n got %s\nwant %s", got, want)
	}
	if text["add_special_tokens"] != true || text["input"] != nil || text["dimensions"] != nil || text["model"] != "google/embeddinggemma-2" {
		t.Errorf("request %s", asJSON(text))
	}
	if got := prefixed(taskDocument, "x"); got != "title: none | text: x" {
		t.Errorf("document prefix %q", got)
	}
}

// TestChunk: chunks of at most chunkSize characters, of whole words, each
// overlapping the one before, together covering the whole text.
func TestChunk(t *testing.T) {
	if got := chunk("  \n "); len(got) != 0 {
		t.Errorf("blank: %q", got)
	}
	if got := chunk(" short text "); len(got) != 1 || got[0] != "short text" {
		t.Errorf("short: %q", got)
	}
	words := make([]string, 2500) // unique words: a chunk's place in the text is unambiguous
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	text := strings.Join(words, " ")
	got := chunk(text)
	if len(got) < 7 {
		t.Fatalf("%d chunks of a %d-character text", len(got), len(text))
	}
	prevEnd := 0
	for i, c := range got {
		at := strings.Index(text, c)
		end := at + len(c)
		wholeWords := at >= 0 && (at == 0 || text[at-1] == ' ') && (end == len(text) || text[end] == ' ')
		switch {
		case !wholeWords:
			t.Fatalf("chunk %d is not whole words of the text: %.30q…", i, c)
		case len(c) > chunkSize:
			t.Errorf("chunk %d has %d characters", i, len(c))
		case i == 0 && at != 0:
			t.Errorf("the first chunk starts at %d", at)
		case i > 0 && (at >= prevEnd || prevEnd-at > chunkOverlap):
			t.Errorf("chunk %d starts at %d; the one before ended at %d: overlap %d", i, at, prevEnd, prevEnd-at)
		}
		prevEnd = end
	}
	if prevEnd != len(text) {
		t.Errorf("the chunks end at %d of %d", prevEnd, len(text))
	}
	noSpaces := strings.Repeat("x", 5000)
	if got := chunk(noSpaces); len(got) != 3 || len(got[0]) != chunkSize {
		t.Errorf("no spaces: %d chunks, first %d", len(got), len(got[0]))
	}
}
