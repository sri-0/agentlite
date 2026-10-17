package images

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"

	"agentlite/internal/artifacts"
	"agentlite/pkg/db/opensearch"
)

// AgentID marks the conversations of the Images page; the UI opens them
// there rather than in the chat.
const AgentID = "images"

type pageRequest struct {
	Prompt   string `json:"prompt"`
	Model    string `json:"model"`    // an image model; "" is the default
	ThreadID string `json:"threadId"` // the page's conversation; "" starts one
	Edit     string `json:"edit"`     // an image of the conversation to edit
}

// Handle is POST /v1/images, the Images page: it makes an image in the page's
// conversation, starting one for the first, and replies {threadId, artifact}.
// A generation is paid for, so it finishes (within its timeout) even if the
// client goes away.
func (s *Studio) Handle(c *echo.Context) error {
	ctx := context.WithoutCancel(c.Request().Context())
	var req pageRequest
	if err := c.Bind(&req); err != nil {
		return err
	}
	thread, err := s.thread(ctx, req)
	if err != nil {
		return err
	}
	img, err := s.Make(ctx, Request{RootID: thread.RootID, ThreadID: thread.ID, Prompt: req.Prompt, Model: req.Model, Edit: req.Edit})
	var upstream *openai.Error
	switch {
	case errors.Is(err, ErrInvalid):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.As(err, &upstream):
		return echo.NewHTTPError(http.StatusBadGateway, "the image model refused: "+upstream.Message).Wrap(err)
	case err != nil:
		return err
	}
	// Saved once there is an image: a failed first one leaves no empty conversation.
	thread.Model, thread.UpdatedAt = cmp.Or(req.Model, s.model), time.Now()
	if err := s.records.SaveThread(ctx, thread); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"threadId": thread.ID, "artifact": artifacts.Summary(img)})
}

// thread is the page's conversation: a new one, titled by the prompt, for
// the first image.
func (s *Studio) thread(ctx context.Context, req pageRequest) (*opensearch.Thread, error) {
	if req.ThreadID == "" {
		id := uuid.Must(uuid.NewV7()).String()
		return &opensearch.Thread{ID: id, RootID: id, Title: title(req.Prompt), AgentID: AgentID, CreatedAt: time.Now()}, nil
	}
	t, err := s.records.Thread(ctx, req.ThreadID)
	if err == nil && t.AgentID != AgentID {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "not an Images conversation")
	}
	return t, err
}
