// Package suggest proposes what the user might send next: a few short
// prompts from the conversation's last message and its answer, as the UI
// sends them, from what the user is typing, or from both, by one call to a
// small model. It stands apart from the agents and reads nothing stored;
// nothing is kept.
package suggest

import (
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
	count   = 3    // suggestions per turn
	maxLen  = 80   // runes of one suggestion
	textCap = 4000 // runes of each message, and of the typed text, the model reads
	turnLen = 2    // last messages the model reads: the message and its answer
	timeout = 30 * time.Second
)

// Errors Suggest returns: a request with neither messages nor text, and a
// model that failed.
var (
	ErrEmpty = errors.New("messages or text is required")
	errModel = errors.New("the suggestions model failed")
)

// Request is what the suggestions start from: the conversation's last turn as
// the UI has it (the last message and the one it answers), what the user is
// typing, or both.
type Request struct {
	Messages []Message `json:"messages"`
	Text     string    `json:"text"`
}

// Message is one message of the conversation.
type Message struct {
	Role    string `json:"role"` // user or assistant
	Content string `json:"content"`
}

// Suggester asks one model for next prompts, resolved on each call so a model
// that discovery adds after start works.
type Suggester struct {
	registry *providers.Registry
	model    string
}

// New returns a suggester that asks model, an LLM.
func New(registry *providers.Registry, model string) *Suggester {
	return &Suggester{registry: registry, model: model}
}

// instruction tells the model what to write and in which shape.
const instruction = `You suggest what the user could send next to an AI assistant. ` +
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
	input := prompt(r.Messages, r.Text)
	if input == "" {
		return nil, ErrEmpty
	}
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
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(instruction), openai.UserMessage(input)},
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

// prompt is what the model reads: the last turnLen messages, then what the
// user is typing, each cut to textCap runes and blank ones left out; "" when
// nothing is left.
func prompt(messages []Message, typed string) string {
	var parts []string
	for _, m := range messages[max(0, len(messages)-turnLen):] {
		if c := strings.TrimSpace(m.Content); c != "" {
			who := "The user wrote:\n"
			if m.Role == "assistant" {
				who = "The assistant answered:\n"
			}
			parts = append(parts, who+clip(c, textCap))
		}
	}
	if t := strings.TrimSpace(typed); t != "" {
		parts = append(parts, "The user is typing:\n"+clip(t, textCap))
	}
	return strings.Join(parts, "\n\n")
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
