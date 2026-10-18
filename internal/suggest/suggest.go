// Package suggest proposes what the user might send next: a few short
// prompts from a thread's last message and answer, from what the user is
// typing, or from both, by one call to a small model. It stands apart from
// the agents; nothing is kept.
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
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/modelcatalog"
)

const (
	count   = 3    // suggestions per turn
	maxLen  = 80   // runes of one suggestion
	textCap = 4000 // runes of the message and of the answer the model reads
	timeout = 30 * time.Second
)

// Errors Suggest returns: a request with neither a thread nor text, a thread
// whose last turn has no finished answer (none yet, or one still running or
// paused) and no text to go on, and a model that failed.
var (
	ErrEmpty    = errors.New("threadId or text is required")
	ErrNoAnswer = errors.New("the thread has no finished answer yet")
	errModel    = errors.New("the suggestions model failed")
)

// Request is what the suggestions start from: a thread, what the user is
// typing, or both.
type Request struct {
	ThreadID string `json:"threadId"`
	Text     string `json:"text"`
}

// History is what the suggestions read: the thread and its last messages.
type History interface {
	Thread(ctx context.Context, id string) (*opensearch.Thread, error)
	Messages(ctx context.Context, threadID string, before int64, limit int) ([]opensearch.Message, error)
}

// Suggester asks one model for next prompts, resolved on each call so a model
// that discovery adds after start works.
type Suggester struct {
	registry *providers.Registry
	history  History
	model    string
}

// New returns a suggester that asks model, an LLM.
func New(registry *providers.Registry, history History, model string) *Suggester {
	return &Suggester{registry: registry, history: history, model: model}
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

// Suggest returns up to three next prompts: from the thread's last answered
// turn, from the text being typed, or both. ErrNoAnswer when there is only a
// thread and its last turn isn't answered.
func (s *Suggester) Suggest(ctx context.Context, r Request) ([]string, error) {
	text := strings.TrimSpace(r.Text)
	if r.ThreadID == "" && text == "" {
		return nil, ErrEmpty
	}
	var message, answer string
	if r.ThreadID != "" {
		if _, err := s.history.Thread(ctx, r.ThreadID); err != nil {
			return nil, err
		}
		msgs, err := s.history.Messages(ctx, r.ThreadID, 0, 2)
		if err != nil {
			return nil, err
		}
		var ok bool
		if message, answer, ok = lastTurn(msgs); !ok && text == "" {
			return nil, ErrNoAnswer
		}
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
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(instruction), openai.UserMessage(prompt(message, answer, text))},
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

// lastTurn is the user's message and the answer to it, when the newest
// message is a finished answer.
func lastTurn(msgs []opensearch.Message) (message, answer string, ok bool) {
	n := len(msgs)
	if n < 2 || msgs[n-1].Role != "assistant" || msgs[n-2].Role != "user" || strings.TrimSpace(msgs[n-1].Text) == "" {
		return "", "", false
	}
	return msgs[n-2].Text, msgs[n-1].Text, true
}

// prompt is what the model reads: the last message and its answer when there
// is one, then what the user is typing when there is any, each cut to textCap
// runes.
func prompt(message, answer, typed string) string {
	var parts []string
	if answer != "" {
		parts = append(parts, "The user wrote:\n"+clip(message, textCap), "The assistant answered:\n"+clip(answer, textCap))
	}
	if typed != "" {
		parts = append(parts, "The user is typing:\n"+clip(typed, textCap))
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

// Handle is POST /v1/suggestions: {threadId?, text?} in, {suggestions: [...]}
// out. Neither is 400; a thread with no answer yet and no text is 409.
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
	case errors.Is(err, ErrNoAnswer):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.As(err, &upstream):
		return echo.NewHTTPError(http.StatusBadGateway, "the suggestions model refused: "+upstream.Message).Wrap(err)
	case errors.Is(err, errModel):
		return echo.NewHTTPError(http.StatusBadGateway, errModel.Error()).Wrap(err)
	case err != nil:
		return err // an unknown thread is 404 (opensearch.ErrNotFound)
	}
	return c.JSON(http.StatusOK, map[string][]string{"suggestions": got})
}
