package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const embedTimeout = 15 * time.Second

// Embed returns the embedding of one text from the provider's /embeddings
// endpoint, through the provider's own HTTP client (mTLS, headers).
func (c *Client) Embed(ctx context.Context, model, input string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	client := openai.NewClient(option.WithBaseURL(c.BaseURL.String()), option.WithAPIKey(c.APIKey), option.WithHTTPClient(c.HTTP))
	resp, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: model, Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String(input)},
	})
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	out := make([]float32, len(resp.Data[0].Embedding))
	for i, v := range resp.Data[0].Embedding {
		out[i] = float32(v)
	}
	return out, nil
}
