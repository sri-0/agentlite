package modelcatalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Parse reads a provider's model list. Two shapes are understood: OpenAI
// /models (also served, with extra metadata, by OpenRouter and vLLM) and
// LiteLLM /model/info, chosen by the endpoint the list came from.
func Parse(endpoint string, body []byte) ([]Model, error) {
	if strings.HasSuffix(endpoint, "/model/info") {
		return parseLiteLLM(body)
	}
	return parseOpenAI(body)
}

type openAIModel struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	OwnedBy             string       `json:"owned_by"`
	Description         string       `json:"description"`
	ContextLength       int          `json:"context_length"`
	MaxModelLen         int          `json:"max_model_len"` // vLLM's name for the context window
	SupportedParameters []string     `json:"supported_parameters"`
	Architecture        Architecture `json:"architecture"`
}

func parseOpenAI(body []byte) ([]Model, error) {
	var payload struct {
		Data []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse /models: %w", err)
	}
	out := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		// Name and Type stay empty unless reported, so a card's show.
		model := Model{
			ID: m.ID, Name: m.Name, OwnedBy: m.OwnedBy, Description: m.Description,
			ContextLength: max(m.ContextLength, m.MaxModelLen), SupportedParameters: m.SupportedParameters,
			Architecture: m.Architecture,
		}
		if strings.Contains(m.ID, "embed") {
			model.Type = "embedding"
		}
		model.Capabilities = capabilitiesFromParameters(m.SupportedParameters, m.Architecture)
		out = append(out, model)
	}
	return out, nil
}

func capabilitiesFromParameters(params []string, arch Architecture) []string {
	caps := []string{}
	for _, p := range params {
		switch p {
		case "tools":
			caps = append(caps, "tool_calling")
		case "reasoning":
			caps = append(caps, "reasoning")
		}
	}
	if slices.Contains(arch.InputModalities, "image") {
		caps = append(caps, "vision")
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

func parseLiteLLM(body []byte) ([]Model, error) {
	var payload struct {
		Data []liteLLMModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse /model/info: %w", err)
	}
	out := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		model := Model{ID: m.ModelName, ContextLength: m.ModelInfo.MaxInputTokens}
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
