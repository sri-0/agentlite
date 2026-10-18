package files

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
)

const (
	embedTimeout = 2 * time.Minute // a whole file's inputs
	batchSize    = 32              // inputs per request, in the deepinfra format
)

// Tasks: what a vector is for. A query is searched with, a document
// searched in; the model embeds each its own way.
const (
	taskQuery    = "search_query"
	taskDocument = "document"
)

// Embedder embeds what files hold (text, images, audio, video) with one
// multimodal model, resolved on each call so a model that discovery adds
// after start works.
type Embedder struct {
	registry *providers.Registry
	model    string
	format   string // the request format: deepinfra or vllm
	dims     int    // the vectors' size; longer ones are cut (Matryoshka)
}

// NewEmbedder returns an embedder for a model of type embedding whose server
// speaks format (deepinfra or vllm).
func NewEmbedder(registry *providers.Registry, model, format string, dims int) *Embedder {
	return &Embedder{registry: registry, model: model, format: format, dims: dims}
}

// input is one thing embedded into one vector: a text, or a file's bytes.
type input struct {
	text      string
	data      []byte
	mediaType string
}

// embed returns a vector per input, in order.
func (e *Embedder) embed(ctx context.Context, task string, inputs []input) ([][]float32, error) {
	t, err := e.registry.ResolveAs(e.model, modelcatalog.TypeEmbedding)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	// Not openai.NewClient: it would add OPENAI_ORG_ID and the like from the
	// environment to a request for another provider.
	embeddings := openai.NewEmbeddingService(option.WithBaseURL(t.BaseURL.String()), option.WithAPIKey(t.Client.APIKey),
		option.WithHTTPClient(t.Client.HTTP))
	out := make([][]float32, 0, len(inputs))
	for _, r := range e.requests(t.Model.UpstreamID, task, inputs) {
		body, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		resp, err := embeddings.New(ctx, openai.EmbeddingNewParams{}, option.WithRequestBody("application/json", body))
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		if len(resp.Data) != r.n {
			return nil, fmt.Errorf("embed: %d vectors for %d inputs", len(resp.Data), r.n)
		}
		vectors := make([][]float32, r.n)
		for _, d := range resp.Data {
			if d.Index < 0 || int(d.Index) >= r.n || vectors[d.Index] != nil {
				return nil, fmt.Errorf("embed: vector index %d out of range or repeated", d.Index)
			}
			vectors[d.Index] = e.vector(d.Embedding)
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// vector is an embedding as stored: float32, cut to the configured size.
func (e *Embedder) vector(v []float64) []float32 {
	out := make([]float32, min(len(v), e.dims))
	for i := range out {
		out[i] = float32(v[i])
	}
	return out
}

// request is one request's body and how many inputs it embeds.
type request struct {
	body any
	n    int
}

// requests are the requests that embed inputs, in order. deepinfra takes a
// batch of inputs, each a text or a list of content parts, and the task; vllm
// takes one input a request, as a chat message, with EmbeddingGemma's task
// prefixes written into its text.
func (e *Embedder) requests(model, task string, inputs []input) []request {
	var out []request
	if e.format == "vllm" {
		for _, in := range inputs {
			content := []any{part(in)}
			if in.data == nil {
				content = []any{map[string]any{"type": "text", "text": prefixed(task, in.text)}}
			}
			out = append(out, request{n: 1, body: map[string]any{
				"model": model, "encoding_format": "float", "add_special_tokens": true,
				"messages": []any{map[string]any{"role": "user", "content": content}},
			}})
		}
		return out
	}
	for start := 0; start < len(inputs); start += batchSize {
		batch := inputs[start:min(start+batchSize, len(inputs))]
		items := make([]any, len(batch))
		for i, in := range batch {
			items[i] = in.text
			if in.data != nil {
				items[i] = []any{part(in)}
			}
		}
		out = append(out, request{n: len(batch), body: map[string]any{
			"model": model, "encoding_format": "float", "dimensions": e.dims, "task": task, "input": items,
		}})
	}
	return out
}

// part is a file as a content part, the way chat requests carry media.
func part(in input) map[string]any {
	encoded := base64.StdEncoding.EncodeToString(in.data)
	dataURL := "data:" + in.mediaType + ";base64," + encoded
	switch kind(in.mediaType) {
	case kindAudio:
		format := "wav"
		if in.mediaType == "audio/mpeg" {
			format = "mp3"
		}
		return map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": encoded, "format": format}}
	case kindVideo:
		return map[string]any{"type": "video_url", "video_url": map[string]any{"url": dataURL}}
	default:
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}
	}
}

// prefixed is a text with EmbeddingGemma's prompt for its task; a server
// that takes the task (deepinfra) adds it itself.
func prefixed(task, text string) string {
	if task == taskQuery {
		return "task: search result | query: " + text
	}
	return "title: none | text: " + text
}
