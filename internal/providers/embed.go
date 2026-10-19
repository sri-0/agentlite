package providers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/pkg/modelcatalog"
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
	resp, err := embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: t.Model.UpstreamID, Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String(text)},
	})
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
