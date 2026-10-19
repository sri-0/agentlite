package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"agentlite/config"
	"agentlite/pkg/modelcatalog"
)

const (
	embedTimeout = 2 * time.Minute // one request: a batch of texts, or a file
	embedBatch   = 64              // texts per request
)

// Embedder embeds with the embedding block's model, for long-term memory and
// the files, through the OpenAI embeddings API. It is the server calling a
// provider itself, so it is not the pass-through proxy.
type Embedder struct {
	registry *Registry
	cfg      config.EmbeddingConfig
}

// Embedder returns the embedder of the embedding block. Its model is
// resolved on each call, so a model that discovery adds after start works.
func (r *Registry) Embedder(cfg config.EmbeddingConfig) *Embedder {
	return &Embedder{registry: r, cfg: cfg}
}

// Model is the embedding model's id, as models.yaml names it.
func (e *Embedder) Model() string { return e.cfg.Model }

// Documents embeds texts to be searched in, a vector each, in order.
func (e *Embedder) Documents(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for batch := range slices.Chunk(texts, embedBatch) {
		prefixed := make([]string, len(batch))
		for i, t := range batch {
			prefixed[i] = e.cfg.DocumentPrefix + t
		}
		vectors, err := e.embed(ctx, openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: prefixed}, len(batch))
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// Query embeds a text to search with.
func (e *Embedder) Query(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.embed(ctx, openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: []string{e.cfg.QueryPrefix + text}}, 1)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// Media embeds an image, audio or video. The OpenAI embeddings API takes only
// text, so input is a chat message holding the file as one content part, the
// way chat requests carry media: vLLM (main since 2026-06) and OpenRouter's
// Gemini embedding both take that, and embed it into one vector.
func (e *Embedder) Media(ctx context.Context, data []byte, mediaType string) ([]float32, error) {
	message := openai.UserMessage([]openai.ChatCompletionContentPartUnionParam{mediaPart(data, mediaType)})
	input := param.Override[openai.EmbeddingNewParamsInputUnion]([]openai.ChatCompletionMessageParamUnion{message})
	vectors, err := e.embed(ctx, input, 1)
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

// mediaPart is a file as a chat content part: an image as image_url and audio
// as input_audio, OpenAI's types; video, which OpenAI has no part for, as
// vLLM's video_url. (OpenRouter takes input_audio only with a data URL, and
// video as input_video, so it embeds neither.)
func mediaPart(data []byte, mediaType string) openai.ChatCompletionContentPartUnionParam {
	encoded := base64.StdEncoding.EncodeToString(data)
	dataURL := "data:" + mediaType + ";base64," + encoded
	switch {
	case strings.HasPrefix(mediaType, "audio/"):
		format := "wav"
		if mediaType == "audio/mpeg" {
			format = "mp3"
		}
		return openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{Data: encoded, Format: format})
	case strings.HasPrefix(mediaType, "video/"):
		return param.Override[openai.ChatCompletionContentPartUnionParam](map[string]any{
			"type": "video_url", "video_url": map[string]string{"url": dataURL},
		})
	default:
		return openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{URL: dataURL})
	}
}

// embed sends one embeddings request for n inputs and returns their vectors.
func (e *Embedder) embed(ctx context.Context, input openai.EmbeddingNewParamsInputUnion, n int) ([][]float32, error) {
	t, err := e.registry.ResolveAs(e.cfg.Model, modelcatalog.TypeEmbedding)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	// Not openai.NewClient: it would add OPENAI_ORG_ID and the like from the
	// environment to a request for another provider.
	embeddings := openai.NewEmbeddingService(option.WithBaseURL(t.BaseURL.String()), option.WithAPIKey(t.Client.APIKey),
		option.WithHTTPClient(t.Client.HTTP))
	resp, err := embeddings.New(ctx, e.params(t.Model.UpstreamID, input))
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(resp.Data) != n {
		return nil, fmt.Errorf("embeddings: %d vectors for %d inputs", len(resp.Data), n)
	}
	out := make([][]float32, n)
	for _, d := range resp.Data {
		if d.Index < 0 || int(d.Index) >= n {
			return nil, fmt.Errorf("embeddings: vector index %d of %d", d.Index, n)
		}
		v := make([]float32, len(d.Embedding))
		for i, x := range d.Embedding {
			v[i] = float32(x)
		}
		out[d.Index] = v // each vector says which input it is
	}
	return out, nil
}

// params is the request: floats, and the configured dimensions unless 0.
func (e *Embedder) params(model string, input openai.EmbeddingNewParamsInputUnion) openai.EmbeddingNewParams {
	p := openai.EmbeddingNewParams{Model: model, Input: input, EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat}
	if e.cfg.Dimensions > 0 {
		p.Dimensions = openai.Int(int64(e.cfg.Dimensions))
	}
	return p
}
