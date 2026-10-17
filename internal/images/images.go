// Package images generates and edits images with an image model, and keeps
// every image as an artifact of its conversation, so each one is on the
// artifacts page. It serves the generate_image tool in a conversation and
// the Images page (POST /v1/images); the agents only see the tool.
package images

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"agentlite/internal/artifacts"
	"agentlite/internal/providers"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/modelcatalog"
)

const (
	timeout  = 3 * time.Minute // an image takes tens of seconds
	titleLen = 80
)

// ErrInvalid is a request no model call can satisfy: no prompt, an unknown
// model or one that makes no images, an edit of what is not an image.
var ErrInvalid = errors.New("invalid image request")

// Studio makes images and keeps them.
type Studio struct {
	registry  *providers.Registry
	artifacts *artifacts.Store
	records   *opensearch.Client
	model     string // the default image model
}

// New returns a studio whose images come from model unless a request names
// another image model.
func New(registry *providers.Registry, store *artifacts.Store, records *opensearch.Client, model string) *Studio {
	return &Studio{registry: registry, artifacts: store, records: records, model: model}
}

// Request is one image to make, in a conversation.
type Request struct {
	RootID, ThreadID, RunID string
	Prompt                  string
	Model                   string // an image model; "" is the default
	// Edit names an image artifact of the conversation to edit; the result
	// is its next version, under its title. Without it the image is a new
	// artifact titled by the prompt.
	Edit string
}

// Make generates the image and saves it as an artifact.
func (s *Studio) Make(ctx context.Context, r Request) (*opensearch.Artifact, error) {
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, fmt.Errorf("%w: describe the image in prompt", ErrInvalid)
	}
	a := &opensearch.Artifact{Kind: artifacts.KindImage, RootID: r.RootID, ThreadID: r.ThreadID, RunID: r.RunID}
	var reference string
	if r.Edit == "" {
		a.ID, a.Title = uuid.Must(uuid.NewV7()).String(), title(r.Prompt)
	} else {
		original, url, err := s.image(ctx, r.RootID, r.Edit)
		if err != nil {
			return nil, err
		}
		a.ID, a.Title, reference = original.ID, original.Title, url
	}
	img, err := s.generate(ctx, cmp.Or(r.Model, s.model), r.Prompt, reference)
	if err != nil {
		return nil, err
	}
	return a, s.artifacts.Save(ctx, a, img)
}

// image is one of the conversation's images, as a data URL of its current
// version.
func (s *Studio) image(ctx context.Context, rootID, id string) (*opensearch.Artifact, string, error) {
	a, err := s.records.Artifact(ctx, rootID, id)
	switch {
	case errors.Is(err, opensearch.ErrNotFound), err == nil && a.Kind != artifacts.KindImage:
		return nil, "", fmt.Errorf("%w: edit: %q is not an image of this conversation", ErrInvalid, id)
	case err != nil:
		return nil, "", err
	}
	body, err := s.artifacts.Body(ctx, a)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	img, err := io.ReadAll(body)
	return a, "data:" + a.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img), err
}

// generate asks the model for one image, through the OpenAI-style
// /images/generations route. A reference image is sent the way OpenRouter
// takes one (input_references); servers that edit through /images/edits
// instead (vLLM-Omni) generate without it. A generation is paid for, so a
// failed one is not retried.
func (s *Studio) generate(ctx context.Context, model, prompt, reference string) ([]byte, error) {
	t, err := s.registry.ResolveAs(model, modelcatalog.TypeImage)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Not openai.NewClient: it would add OPENAI_ORG_ID and the like from the
	// environment to a request for another provider.
	images := openai.NewImageService(
		option.WithBaseURL(t.BaseURL.String()),
		option.WithAPIKey(t.Client.APIKey),
		option.WithHTTPClient(t.Client.HTTP),
		option.WithMaxRetries(0),
	)
	var opts []option.RequestOption
	if reference != "" {
		opts = append(opts, option.WithJSONSet("input_references", []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}},
		}))
	}
	resp, err := images.Generate(ctx, openai.ImageGenerateParams{Prompt: prompt, Model: t.Model.UpstreamID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("generate image: %w", err)
	}
	if len(resp.Data) == 0 || resp.Data[0].B64JSON == "" {
		return nil, errors.New("generate image: the model returned no image")
	}
	return base64.StdEncoding.DecodeString(resp.Data[0].B64JSON)
}

// title is a prompt's first line, cut to titleLen runes.
func title(prompt string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	t := []rune(strings.TrimSpace(first))
	if len(t) > titleLen {
		return string(t[:titleLen-1]) + "…"
	}
	return string(t)
}
