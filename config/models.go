package config

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentlite/pkg/modelcatalog"
)

// ModelsConfig is config/default/models.yaml: the upstream providers and the
// models they serve. A model id is "<provider>/<model>": the provider's id,
// then the provider's own name for the model.
type ModelsConfig struct {
	Providers    []Provider       `yaml:"providers"`
	Embedding    *EmbeddingConfig `yaml:"embedding"`     // nil: no long-term memory, files not searchable
	Memory       *MemoryConfig    `yaml:"memory"`        // long-term memory; needs the embedding block
	DefaultModel string           `yaml:"default_model"` // answers when a request names no model
	Compaction   *Compaction      `yaml:"compaction"`    // nil disables context compaction
	Voice        *VoiceConfig     `yaml:"voice"`         // nil disables the voice prompt
	Images       *ImagesConfig    `yaml:"images"`        // nil disables image generation
	Files        FilesConfig      `yaml:"files"`         // uploaded files: limits and search
	// Suggestions names the model that proposes next prompts after a turn
	// (internal/suggest); nil uses default_model.
	Suggestions *SuggestionsConfig `yaml:"suggestions"`
}

// Compaction configures adk's session compaction for every run. Tail
// retention is the main strategy: before a model call whose prompt passes
// TokenThreshold of the model's context window, everything but the newest
// KeepEvents events is summarised. A sliding window (a summary every Interval
// user turns, keeping Overlap of them) can run too; it runs after the answer.
type Compaction struct {
	TokenThreshold    float64       `yaml:"token_threshold"` // 0 turns tail retention off
	KeepEvents        int           `yaml:"keep_events"`     // default 20
	Interval          int           `yaml:"interval"`        // 0 turns the sliding window off
	Overlap           int           `yaml:"overlap"`
	SummarizerTimeout time.Duration `yaml:"summarizer_timeout"` // default 60s
}

// Compaction defaults.
const (
	defaultKeepEvents        = 20
	defaultSummarizerTimeout = 60 * time.Second
)

func (c *Compaction) check() error {
	if c.KeepEvents == 0 {
		c.KeepEvents = defaultKeepEvents
	}
	if c.SummarizerTimeout == 0 {
		c.SummarizerTimeout = defaultSummarizerTimeout
	}
	switch {
	case c.TokenThreshold < 0 || c.TokenThreshold > 1:
		return errors.New("token_threshold is a share of the context window, within 0..1")
	case c.TokenThreshold == 0 && c.Interval == 0:
		return errors.New("set token_threshold or interval, or remove the block")
	case c.Interval < 0 || c.Overlap < 0 || (c.Interval > 0 && c.Overlap >= c.Interval):
		return errors.New("interval > overlap >= 0")
	case c.KeepEvents < 1:
		return errors.New("keep_events is at least 1")
	}
	return nil
}

// EmbeddingConfig is the one embedding model, for long-term memory and the
// files: the memories and userfile_chunks indexes are sized for its vectors
// (deploy/opensearch). It is called through the OpenAI
// embeddings API (docs/MODELS.md). Dimensions is sent as the request's
// dimensions; 0 leaves it out, for a model whose vectors are that size anyway
// (vLLM refuses dimensions for EmbeddingGemma 2, vllm#60977). The prefixes go
// before every text embedded to search with (query) and to search in
// (document), for a model trained with them; empty, none.
type EmbeddingConfig struct {
	Model          string `yaml:"model"`
	Dimensions     int    `yaml:"dimensions"`
	QueryPrefix    string `yaml:"query_prefix"`
	DocumentPrefix string `yaml:"document_prefix"`
	// Index is the OpenSearch index of the files' chunks and their vectors,
	// without the deployment's prefix; default userfile_chunks.
	Index string `yaml:"index"`
}

// MemoryConfig enables long-term memory, embedded with the embedding block's
// model.
type MemoryConfig struct {
	// Preload searches memory before every model call of a user-started run;
	// off, the agent decides when to search with the load_memory tool.
	Preload bool `yaml:"preload"`
	// Index is the OpenSearch index of the memories, without the deployment's
	// prefix; default memories.
	Index string `yaml:"index"`
}

// VoiceConfig enables the voice prompt: the transcription model that turns a
// recording into the text of a message (internal/voice).
type VoiceConfig struct {
	Model   string        `yaml:"model"`
	MaxMB   int           `yaml:"max_mb"`  // the largest recording; default 25, the providers' own limit
	Timeout time.Duration `yaml:"timeout"` // one transcription; default 60s
}

// Voice and images defaults.
const (
	defaultVoiceMaxMB    = 25
	defaultVoiceTimeout  = 60 * time.Second
	defaultImagesTimeout = 3 * time.Minute
)

// ImagesConfig enables image generation (internal/images): the image model
// the generate_image tool uses, and the Images page's default.
type ImagesConfig struct {
	Model string `yaml:"model"`
	// EditModel edits on the OpenAI /images/edits route, the original
	// uploaded in a multipart form, as vLLM-Omni serves it: Qwen-Image 2.1
	// makes and edits, so it is Model again; earlier Qwen-Image models edit
	// with Qwen-Image-Edit, on a server of its own. Without it an edit goes
	// to Model on /images/generations, the original in OpenRouter's
	// input_references (development).
	EditModel string        `yaml:"edit_model"`
	Timeout   time.Duration `yaml:"timeout"` // one image; default 3m (an image takes tens of seconds)
}

// SuggestionsConfig is the small LLM that proposes what the user might send
// next once a turn is answered (internal/suggest), and its system prompt
// ("": the built-in one).
type SuggestionsConfig struct {
	Model  string `yaml:"model"`
	Prompt string `yaml:"prompt"`
}

// FilesConfig is the files block (internal/rag/userfiles, docs/FILES.md): limits on
// the files users upload and on what a #reference puts in the model's context
// as it is, and how files are embedded and searched. Files are searchable
// only with the embedding block; without it they are kept, not searchable. A
// field left out takes its default.
type FilesConfig struct {
	MaxUploadMB   int `yaml:"max_upload_mb"`   // the largest upload; default 25
	InlineTextKB  int `yaml:"inline_text_kb"`  // a text up to this goes inline; default 20
	InlineMediaMB int `yaml:"inline_media_mb"` // an image or PDF up to this goes as it is; default 10
	MaxTags       int `yaml:"max_tags"`        // default 10
	MaxTagChars   int `yaml:"max_tag_chars"`   // default 40
	// A text file is embedded in chunks of about ChunkChars characters, each
	// overlapping the last by ChunkOverlap so a passage cut in two is whole in
	// one of them; one over SearchableTextMB is refused (it would take minutes).
	ChunkChars       int `yaml:"chunk_chars"`        // default 2000
	ChunkOverlap     int `yaml:"chunk_overlap"`      // default 200
	SearchableTextMB int `yaml:"searchable_text_mb"` // default 2
	SearchHits       int `yaml:"search_hits"`        // what search_userfiles returns; default 8
	// Up to SearchMedia images and PDFs that search_userfiles finds are shown
	// to a model that sees images, on the call that reads its result.
	SearchMedia int `yaml:"search_media"` // default 3
}

// Provider is one OpenAI-compatible upstream. Secrets are named by environment
// variable; base_url and header values may use ${ENV} expansion.
type Provider struct {
	ID        string            `yaml:"id"`
	Name      string            `yaml:"name"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	API       string            `yaml:"api"` // chat_completions (default) | responses
	Headers   map[string]string `yaml:"headers"`
	Discover  *Discover         `yaml:"discover"`
	// Models are declared by the provider's own id; each overrides, field by
	// field, what discovery and a model card say about that model, and may
	// set its own base_url and api.
	Models []modelcatalog.Model `yaml:"models"`
	TLS    *TLS                 `yaml:"tls"` // mutual TLS and trusted authorities, from env-named files
}

// Discover fetches the provider's model list and merges the YAML models over it.
type Discover struct {
	Endpoint string        `yaml:"endpoint"` // relative to base_url: /models or /model/info
	Refresh  time.Duration `yaml:"refresh"`
}

// APIs a provider or model may name; "" is chat_completions.
var apis = []string{"", "chat_completions", "responses"}

// loadModels reads models.yaml and expands ${ENV} references.
func loadModels(dir string) (*ModelsConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	var cfg ModelsConfig
	if err := decode(raw, &cfg); err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	if cfg.DefaultModel == "" {
		return nil, errors.New("models.yaml: default_model is required")
	}
	f := &cfg.Files
	f.MaxUploadMB, f.InlineTextKB, f.InlineMediaMB = cmp.Or(f.MaxUploadMB, 25), cmp.Or(f.InlineTextKB, 20), cmp.Or(f.InlineMediaMB, 10)
	f.MaxTags, f.MaxTagChars = cmp.Or(f.MaxTags, 10), cmp.Or(f.MaxTagChars, 40)
	f.ChunkChars, f.ChunkOverlap, f.SearchableTextMB = cmp.Or(f.ChunkChars, 2000), cmp.Or(f.ChunkOverlap, 200), cmp.Or(f.SearchableTextMB, 2)
	f.SearchHits, f.SearchMedia = cmp.Or(f.SearchHits, 8), cmp.Or(f.SearchMedia, 3)
	if c := cfg.Compaction; c != nil {
		if err := c.check(); err != nil {
			return nil, fmt.Errorf("models.yaml: compaction: %w", err)
		}
	}
	if v := cfg.Voice; v != nil {
		v.MaxMB, v.Timeout = cmp.Or(v.MaxMB, defaultVoiceMaxMB), cmp.Or(v.Timeout, defaultVoiceTimeout)
	}
	if i := cfg.Images; i != nil {
		i.Timeout = cmp.Or(i.Timeout, defaultImagesTimeout)
	}
	if cfg.Memory != nil && cfg.Embedding == nil {
		return nil, errors.New("models.yaml: memory needs the embedding block")
	}
	seen := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		switch {
		case p.ID == "" || strings.Contains(p.ID, "/"):
			return nil, fmt.Errorf("models.yaml: provider id %q must be set and contain no /", p.ID)
		case seen[p.ID]:
			return nil, fmt.Errorf("models.yaml: duplicate provider %q", p.ID)
		case !slices.Contains(apis, p.API):
			return nil, fmt.Errorf("models.yaml: provider %s: api %q is not chat_completions or responses", p.ID, p.API)
		}
		seen[p.ID] = true
		// A provider with no base_url here is skipped by the registry, and
		// its headers are not needed.
		if p.BaseURL = os.ExpandEnv(p.BaseURL); p.BaseURL != "" {
			if err := expandEnv(p.Headers); err != nil {
				return nil, fmt.Errorf("models.yaml: provider %s: %w", p.ID, err)
			}
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		for j := range p.Models {
			m := &p.Models[j]
			if !slices.Contains(apis, m.API) {
				return nil, fmt.Errorf("models.yaml: model %s/%s: api %q is not chat_completions or responses", p.ID, m.ID, m.API)
			}
			// Checked by the providers registry; one whose variable is unset
			// must not quietly become the provider's own URL.
			if raw := m.BaseURL; raw != "" {
				if m.BaseURL = os.ExpandEnv(raw); m.BaseURL == "" {
					return nil, fmt.Errorf("models.yaml: model %s/%s: base_url %s is empty in this environment", p.ID, m.ID, raw)
				}
			}
		}
	}
	return &cfg, nil
}
