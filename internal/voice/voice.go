// Package voice turns a spoken prompt into text with a transcription model
// such as Voxtral or Whisper. It stands apart from the agents: the client
// puts the text in its composer, and it is sent as if it had been typed.
package voice

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/config"
	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
)

// Transcriber transcribes recordings with one model, resolved on each call
// so a model that discovery adds after start works.
type Transcriber struct {
	registry *providers.Registry
	cfg      config.VoiceConfig
}

// New returns a transcriber for the voice block's model, of type transcription.
func New(registry *providers.Registry, cfg config.VoiceConfig) *Transcriber {
	return &Transcriber{registry: registry, cfg: cfg}
}

// Transcribe returns what a recording says. Its file name's extension tells
// the provider the format.
func (t *Transcriber) Transcribe(ctx context.Context, audio io.Reader, filename, contentType string) (string, error) {
	target, err := t.registry.ResolveAs(t.cfg.Model, modelcatalog.TypeTranscription)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	// Not openai.NewClient: it would add OPENAI_ORG_ID and the like from the
	// environment to a request for another provider.
	transcriptions := openai.NewAudioTranscriptionService(
		option.WithBaseURL(target.BaseURL.String()),
		option.WithAPIKey(target.Client.APIKey),
		option.WithHTTPClient(target.Client.HTTP),
	)
	resp, err := transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  openai.File(audio, filename, contentType),
		Model: target.Model.UpstreamID,
	})
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return strings.TrimSpace(resp.Text), nil
}
