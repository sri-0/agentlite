// Package modelcatalog is what agentlite knows about models: the shape it
// serves, the parsers for what providers report, model cards, and how the
// three are merged. It imports nothing from agentlite.
package modelcatalog

import (
	"maps"

	"dario.cat/mergo"
)

// Model is one model as GET /v1/models serves it. In models.yaml and in a
// model card, ID is the upstream id (the provider's own name for it); served,
// it is "<provider>/<upstream id>".
type Model struct {
	ID         string `yaml:"id" json:"id"`
	UpstreamID string `yaml:"-" json:"upstream_id"`

	Name                string             `yaml:"name" json:"name,omitempty"`
	Type                string             `yaml:"type" json:"type"` // one of the Type constants
	OwnedBy             string             `yaml:"owned_by" json:"owned_by,omitempty"`
	Description         string             `yaml:"description" json:"description,omitempty"`
	ContextLength       int                `yaml:"context_length" json:"context_length,omitempty"`
	MaxOutputTokens     int                `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
	Capabilities        []string           `yaml:"capabilities" json:"capabilities,omitempty"`
	SupportedParameters []string           `yaml:"supported_parameters" json:"supported_parameters,omitempty"`
	Architecture        Architecture       `yaml:"architecture" json:"architecture,omitzero"`
	Pricing             Pricing            `yaml:"pricing" json:"pricing,omitzero"`
	Benchmarks          map[string]float64 `yaml:"benchmarks" json:"benchmarks,omitempty"`
	License             string             `yaml:"license" json:"license,omitempty"`
	ReleaseDate         string             `yaml:"release_date" json:"release_date,omitempty"`
	KnowledgeCutoff     string             `yaml:"knowledge_cutoff" json:"knowledge_cutoff,omitempty"`
	Sources             []string           `yaml:"sources" json:"sources,omitempty"`
	// Notes is a model card's body: what the model is good at, its limits.
	Notes string `yaml:"-" json:"notes,omitempty"`
	// Aliases are other ids providers use for the model (cards only).
	Aliases []string `yaml:"aliases" json:"-"`

	// BaseURL and API override the provider's for this model (models.yaml
	// only); the provider's key and client certificate go with them.
	BaseURL string `yaml:"base_url" json:"-"`
	API     string `yaml:"api" json:"-"`

	ProviderID   string `yaml:"-" json:"provider_id"`
	ProviderName string `yaml:"-" json:"provider_name"`
}

// Model types: what a model does, which decides the endpoints it may be sent
// to and whether agents can run on it. A provider can report others (video,
// say); no endpoint takes those.
const (
	TypeLLM           = "llm"           // chat completions, responses, messages
	TypeEmbedding     = "embedding"     // embeddings
	TypeRerank        = "rerank"        // relevance scores for documents
	TypeImage         = "image"         // image generation and editing
	TypeAudio         = "audio"         // speech and audio understanding through chat
	TypeTranscription = "transcription" // speech to text
	TypeSpeech        = "speech"        // text to speech
	TypeDecision      = "decision"      // typed answers to questions
)

// Architecture mirrors the OpenRouter model metadata shape.
type Architecture struct {
	InputModalities  []string `yaml:"input_modalities" json:"input_modalities,omitempty"`
	OutputModalities []string `yaml:"output_modalities" json:"output_modalities,omitempty"`
	Tokenizer        string   `yaml:"tokenizer" json:"tokenizer,omitempty"`
	InstructType     string   `yaml:"instruct_type" json:"instruct_type,omitempty"`
}

// Pricing is in US dollars per million tokens.
type Pricing struct {
	Input      float64 `yaml:"input" json:"input,omitempty"`
	Output     float64 `yaml:"output" json:"output,omitempty"`
	CacheRead  float64 `yaml:"cache_read" json:"cache_read,omitempty"`
	CacheWrite float64 `yaml:"cache_write" json:"cache_write,omitempty"`
}

// Merge layers what is known about one model, lowest precedence first: a
// card, then what the provider reports, then models.yaml. A field set in a
// later layer wins; an empty one leaves the earlier value (so a layer cannot
// clear a value, only replace it).
func Merge(layers ...Model) Model {
	var out Model
	for _, l := range layers {
		l.Benchmarks = maps.Clone(l.Benchmarks)      // never merge into a card's own map
		_ = mergo.Merge(&out, l, mergo.WithOverride) // Model has no field mergo cannot merge
	}
	return out
}
