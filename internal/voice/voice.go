// Package voice turns a spoken prompt into text with a transcription model
// such as Voxtral or Whisper. It stands apart from the agents: the client
// puts the text in its composer, and it is sent as if it had been typed.
package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
)

const (
	// MaxBytes is the largest recording accepted, the providers' own limit.
	MaxBytes = 25 << 20
	timeout  = 60 * time.Second
)

// Transcriber transcribes recordings with one model, resolved on each call
// so a model that discovery adds after start works.
type Transcriber struct {
	registry *providers.Registry
	model    string
}

// New returns a transcriber for a model of type transcription.
func New(registry *providers.Registry, model string) *Transcriber {
	return &Transcriber{registry: registry, model: model}
}

// Transcribe returns what a recording says. Its file name's extension tells
// the provider the format.
func (t *Transcriber) Transcribe(ctx context.Context, audio io.Reader, filename, contentType string) (string, error) {
	target, err := t.registry.ResolveAs(t.model, modelcatalog.TypeTranscription)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
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

// Handle is POST /v1/voice/transcriptions: a multipart form whose file is the
// recording; the reply is {text}.
func (t *Transcriber) Handle(c *echo.Context) error {
	file, header, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the recording as file").Wrap(err)
	}
	defer file.Close()
	text, err := t.Transcribe(c.Request().Context(), file, header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "transcription failed").Wrap(err)
	}
	return c.JSON(http.StatusOK, map[string]string{"text": text})
}
