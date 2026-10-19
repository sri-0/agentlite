package handler

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"

	"agentlite/internal/suggest"
)

// Suggestions is POST /v1/suggestions: {messages?, text?} in, {suggestions:
// [...]} out (internal/suggest). Neither is 400.
func (h *Handler) Suggestions(c *echo.Context) error {
	var r suggest.Request
	if err := c.Bind(&r); err != nil {
		return err
	}
	got, err := h.Suggest.Suggest(c.Request().Context(), r)
	var upstream *openai.Error
	switch {
	case errors.Is(err, suggest.ErrEmpty):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.As(err, &upstream):
		return echo.NewHTTPError(http.StatusBadGateway, "the suggestions model refused: "+upstream.Message).Wrap(err)
	case errors.Is(err, suggest.ErrModel):
		return echo.NewHTTPError(http.StatusBadGateway, suggest.ErrModel.Error()).Wrap(err)
	case err != nil:
		return err
	}
	return c.JSON(http.StatusOK, map[string][]string{"suggestions": got})
}
