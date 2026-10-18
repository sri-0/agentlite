package files

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
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
	size := batchSize
	if e.format == "vllm" {
		size = 1 // vLLM's chat-style embeddings take one input a request
	}
	out := make([][]float32, 0, len(inputs))
	for batch := range slices.Chunk(inputs, size) {
		body, err := json.Marshal(e.body(t.Model.UpstreamID, task, batch))
		if err != nil {
			return nil, err
		}
		resp, err := embeddings.New(ctx, openai.EmbeddingNewParams{}, option.WithRequestBody("application/json", body))
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		if len(resp.Data) != len(batch) {
			return nil, fmt.Errorf("embed: %d vectors for %d inputs", len(resp.Data), len(batch))
		}
		vectors := make([][]float32, len(batch))
		for _, d := range resp.Data {
			if d.Index < 0 || int(d.Index) >= len(batch) || vectors[d.Index] != nil {
				return nil, fmt.Errorf("embed: vector index %d out of range or repeated", d.Index)
			}
			v := make([]float32, min(len(d.Embedding), e.dims)) // cut to the configured size (Matryoshka)
			for i := range v {
				v[i] = float32(d.Embedding[i])
			}
			vectors[d.Index] = v
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// body is the request that embeds a batch. deepinfra takes the batch, each
// input a text or a list with one content part, and the task; vllm takes one
// input as a chat message, with EmbeddingGemma's task prompt written into a
// text.
func (e *Embedder) body(model, task string, batch []input) map[string]any {
	if e.format == "vllm" {
		content := map[string]any{"type": "text", "text": prefixed(task, batch[0].text)}
		if batch[0].data != nil {
			content = part(batch[0])
		}
		return map[string]any{
			"model": model, "encoding_format": "float", "add_special_tokens": true,
			"messages": []any{map[string]any{"role": "user", "content": []any{content}}},
		}
	}
	items := make([]any, len(batch))
	for i, in := range batch {
		items[i] = in.text
		if in.data != nil {
			items[i] = []any{part(in)}
		}
	}
	return map[string]any{"model": model, "encoding_format": "float", "dimensions": e.dims, "task": task, "input": items}
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
