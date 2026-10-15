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
			ID: m.ID, Name: m.Name, OwnedBy: m.OwnedBy,
			Description: m.Description, ContextLength: max(m.ContextLength, m.MaxModelLen),
			SupportedParameters: m.SupportedParameters, Architecture: m.Architecture,
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
		model := Model{ID: m.ModelName, Type: liteLLMType(m.ModelInfo.Mode), ContextLength: m.ModelInfo.MaxInputTokens}
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

// liteLLMType maps LiteLLM's model_info.mode to a type. No mode is "" (an
// llm by default, as LiteLLM treats it); a mode with no type of ours
// (video_generation, realtime, ocr, ...) stays as it is, which no endpoint
// takes.
func liteLLMType(mode string) string {
	switch mode {
	case "chat", "completion", "responses":
		return TypeLLM
	case "embedding":
		return TypeEmbedding
	case "rerank":
		return TypeRerank
	case "image_generation", "image_edit":
		return TypeImage
	case "audio_transcription":
		return TypeTranscription
	case "audio_speech":
		return TypeSpeech
	case "decisions":
		return TypeDecision
	}
	return mode
}
