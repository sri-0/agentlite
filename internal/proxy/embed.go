package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Embed returns the embedding of one text from the provider's /embeddings endpoint.
func (c *Client) Embed(ctx context.Context, model, input string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": input})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL.String()+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := c.Authorization(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", resp.StatusCode, raw)
	}
	var payload struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload.Data) == 0 {
		return nil, fmt.Errorf("embeddings: unexpected response")
	}
	return payload.Data[0].Embedding, nil
}
