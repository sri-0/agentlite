package providers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const embedTimeout = 15 * time.Second

// Embedder embeds text with one model. It is the server calling a provider
// itself (long-term memory), so it is not the pass-through proxy.
type Embedder struct {
	registry *Registry
	model    string
}

// Embedder returns an embedder for a model id. The id is resolved on each
// call, so a model that discovery adds after start works.
func (r *Registry) Embedder(model string) *Embedder {
	return &Embedder{registry: r, model: model}
}

// Embed returns the embedding of one text, sending the provider its own id.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	t, err := e.registry.Resolve(e.model)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	resp, err := t.Client.openai.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: t.Model.UpstreamID, Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String(text)},
	}, option.WithBaseURL(t.BaseURL.String()))
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, errors.New("embeddings: empty response")
	}
	out := make([]float32, len(resp.Data[0].Embedding))
	for i, v := range resp.Data[0].Embedding {
		out[i] = float32(v)
	}
	return out, nil
}
