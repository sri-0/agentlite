package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentlite/internal/config"
)

// discoverModels fetches the provider's model list. Two shapes are understood:
// OpenAI /models (also served with extra metadata by OpenRouter and vLLM) and
// LiteLLM /model/info.
func discoverModels(ctx context.Context, c *Client) ([]config.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL.String()+c.Config.Discover.Endpoint, nil)
	if err != nil {
		return nil, err
	}
	if auth := c.Authorization(); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	if strings.HasSuffix(c.Config.Discover.Endpoint, "/model/info") {
		return parseLiteLLM(body)
	}
	return parseOpenAI(body)
}

type openAIModel struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	OwnedBy             string               `json:"owned_by"`
	Description         string               `json:"description"`
	ContextLength       int                  `json:"context_length"`
	SupportedParameters []string             `json:"supported_parameters"`
	Architecture        *config.Architecture `json:"architecture"`
}

func parseOpenAI(body []byte) ([]config.Model, error) {
	var payload struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	out := make([]config.Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		model := config.Model{
			ID: m.ID, Name: m.Name, Type: "llm", OwnedBy: m.OwnedBy, Description: m.Description,
			ContextLength: m.ContextLength, SupportedParameters: m.SupportedParameters, Architecture: m.Architecture,
		}
		if strings.Contains(m.ID, "embed") {
			model.Type = "embedding"
		}
		if m.SupportedParameters != nil {
			model.Capabilities = capabilitiesFromParameters(m.SupportedParameters, m.Architecture)
		}
		out = append(out, model)
	}
	return out, nil
}

func capabilitiesFromParameters(params []string, arch *config.Architecture) []string {
	var caps []string
	for _, p := range params {
		switch p {
		case "tools":
			caps = append(caps, "tool_calling")
		case "reasoning":
			caps = append(caps, "reasoning")
		}
	}
	if arch != nil {
		for _, m := range arch.InputModalities {
			if m == "image" {
				caps = append(caps, "vision")
			}
		}
	}
	return caps
}

type liteLLMModel struct {
	ModelName string `json:"model_name"`
	ModelInfo struct {
		Mode                    string `json:"mode"`
		MaxInputTokens          int    `json:"max_input_tokens"`
		SupportsFunctionCalling bool   `json:"supports_function_calling"`
		SupportsReasoning       bool   `json:"supports_reasoning"`
		SupportsVision          bool   `json:"supports_vision"`
	} `json:"model_info"`
}

func parseLiteLLM(body []byte) ([]config.Model, error) {
	var payload struct {
		Data []liteLLMModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse /model/info: %w", err)
	}
	out := make([]config.Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		model := config.Model{ID: m.ModelName, Name: m.ModelName, Type: "llm", ContextLength: m.ModelInfo.MaxInputTokens}
		if m.ModelInfo.Mode == "embedding" {
			model.Type = "embedding"
		}
		if m.ModelInfo.SupportsFunctionCalling {
			model.Capabilities = append(model.Capabilities, "tool_calling")
		}
		if m.ModelInfo.SupportsReasoning {
			model.Capabilities = append(model.Capabilities, "reasoning")
			model.SupportedParameters = append(model.SupportedParameters, "reasoning_effort")
		}
		if m.ModelInfo.SupportsVision {
			model.Capabilities = append(model.Capabilities, "vision")
		}
		out = append(out, model)
	}
	return out, nil
}
