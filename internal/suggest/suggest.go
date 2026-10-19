// Package suggest proposes what the user might send next: a few short
// prompts from the conversation's last message and its answer, as the UI
// sends them, from what the user is typing, or from both, by one call to a
// small model. It stands apart from the agents and reads nothing stored;
// nothing is kept.
package suggest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
)

const (
	count   = 3  // suggestions per turn
	maxLen  = 80 // runes of one suggestion
	timeout = 30 * time.Second
)

// Errors Suggest returns: a request with neither messages nor text, and a
// model that failed.
var (
	ErrEmpty = errors.New("messages or text is required")
	errModel = errors.New("the suggestions model failed")
)

// Request is what the suggestions start from: the conversation's last turn as
// the UI has it (the last message and the one it answers, as OpenAI chat
// messages: [{role, content}]), what the user is typing, or both.
type Request struct {
	Messages []openai.ChatCompletionMessageParamUnion `json:"messages"`
	Text     string                                   `json:"text"`
}

// Suggester asks one model for next prompts, resolved on each call so a model
// that discovery adds after start works.
type Suggester struct {
	registry *providers.Registry
	model    string
	prompt   string // the system prompt
}

// New returns a suggester that asks model, an LLM, with the system prompt
// prompt ("": defaultPrompt).
func New(registry *providers.Registry, model, prompt string) *Suggester {
	return &Suggester{registry: registry, model: model, prompt: cmp.Or(prompt, defaultPrompt)}
}

// defaultPrompt tells the model what to write and in which shape;
// suggestions.prompt in models.yaml replaces it.
const defaultPrompt = `You suggest what the user could send next to an AI assistant. ` +
	`Write exactly three short messages in the user's own voice, as they would type them: ` +
	`each a different direction, at most twelve words, no numbering, no quotes. ` +
	`When the user has started typing, each one completes or sharpens what they typed; ` +
	`otherwise they follow from the last answer. ` +
	`Reply with JSON: {"suggestions": ["…", "…", "…"]}.`

// schema is the reply's shape, for providers that hold the model to it.
var schema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"suggestions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
	"required":             []string{"suggestions"},
	"additionalProperties": false,
}

// Suggest returns up to three next prompts: from the last turn, from the text
// being typed, or both.
func (s *Suggester) Suggest(ctx context.Context, r Request) ([]string, error) {
	text := strings.TrimSpace(r.Text)
	if len(r.Messages) == 0 && text == "" {
		return nil, ErrEmpty
	}
	// The turn goes to the model as it came; a last message asks for the
	// suggestions, so the conversation never ends on the answer.
	ask := "Suggest the user's next message."
	if text != "" {
		ask = "The user is typing:\n" + text
	}
	messages := append([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage(s.prompt)}, r.Messages...)
	target, err := s.registry.ResolveAs(s.model, modelcatalog.TypeLLM)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Not openai.NewClient: it would add OPENAI_ORG_ID and the like from the
	// environment to a request for another provider.
	chat := openai.NewChatCompletionService(
		option.WithBaseURL(target.BaseURL.String()),
		option.WithAPIKey(target.Client.APIKey),
		option.WithHTTPClient(target.Client.HTTP),
	)
	resp, err := chat.New(ctx, openai.ChatCompletionNewParams{
		Model:    target.Model.UpstreamID,
		Messages: append(messages, openai.UserMessage(ask)),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
			JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: "suggestions", Schema: schema, Strict: openai.Bool(true)},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errModel, err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%w: it returned nothing", errModel)
	}
	return parse(resp.Choices[0].Message.Content)
}

// parse reads the model's JSON: at most three suggestions, trimmed, cut to
// maxLen runes, empty ones dropped.
func parse(content string) ([]string, error) {
	var reply struct {
		Suggestions []string `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(content), &reply); err != nil {
		return nil, fmt.Errorf("%w: its reply isn't the JSON asked for: %w", errModel, err)
	}
	out := make([]string, 0, count)
	for _, s := range reply.Suggestions {
		if s = strings.TrimSpace(s); s != "" && len(out) < count {
			out = append(out, clip(s, maxLen))
		}
	}
	return out, nil
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// Handle is POST /v1/suggestions: {messages?, text?} in, {suggestions: [...]}
// out. Neither is 400.
func (s *Suggester) Handle(c *echo.Context) error {
	var r Request
	if err := c.Bind(&r); err != nil {
		return err
	}
	got, err := s.Suggest(c.Request().Context(), r)
	var upstream *openai.Error
	switch {
	case errors.Is(err, ErrEmpty):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.As(err, &upstream):
		return echo.NewHTTPError(http.StatusBadGateway, "the suggestions model refused: "+upstream.Message).Wrap(err)
	case errors.Is(err, errModel):
		return echo.NewHTTPError(http.StatusBadGateway, errModel.Error()).Wrap(err)
	case err != nil:
		return err
	}
	return c.JSON(http.StatusOK, map[string][]string{"suggestions": got})
}
