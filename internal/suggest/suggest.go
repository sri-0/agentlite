// Package suggest proposes what the user might send next once a turn is
// over: a few short prompts from the thread's last message and answer, by one
// call to a small model. It stands apart from the agents; nothing is kept.
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

// Errors Suggest returns: a thread whose last turn has no finished answer
// (none yet, or one still running or paused), and a model that failed.
var (
	ErrNoAnswer = errors.New("the thread has no finished answer yet")
	errModel    = errors.New("the suggestions model failed")
)

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
const instruction = `You suggest what the user could send next in this conversation. ` +
	`Write exactly three short follow-up messages in the user's own voice, as they would type them: ` +
	`specific to the answer, each a different direction, at most ten words, no numbering, no quotes. ` +
	`Reply with JSON: {"suggestions": ["…", "…", "…"]}.`

// schema is the reply's shape, for providers that hold the model to it.
var schema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"suggestions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
	"required":             []string{"suggestions"},
	"additionalProperties": false,
}

// Suggest returns up to three next prompts for a thread; ErrNoAnswer when its
// last turn isn't answered.
func (s *Suggester) Suggest(ctx context.Context, threadID string) ([]string, error) {
	if _, err := s.history.Thread(ctx, threadID); err != nil {
		return nil, err
	}
	msgs, err := s.history.Messages(ctx, threadID, 0, 2)
	if err != nil {
		return nil, err
	}
	message, answer, ok := lastTurn(msgs)
	if !ok {
		return nil, ErrNoAnswer
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
		Messages: []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(instruction), openai.UserMessage(prompt(message, answer))},
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

// prompt is what the model reads: the last message and its answer, each cut
// to textCap runes.
func prompt(message, answer string) string {
	return "The user wrote:\n" + clip(message, textCap) + "\n\nThe assistant answered:\n" + clip(answer, textCap)
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

// Handle is POST /v1/threads/:id/suggestions: {suggestions: [...]}. A thread
// whose last turn isn't answered yet is 409.
func (s *Suggester) Handle(c *echo.Context) error {
	got, err := s.Suggest(c.Request().Context(), c.Param("id"))
	var upstream *openai.Error
	switch {
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
