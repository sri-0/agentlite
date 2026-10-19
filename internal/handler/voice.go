package handler

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Transcribe is POST /v1/voice/transcriptions: a multipart form whose file is
// the recording; the reply is {text} (internal/voice).
func (h *Handler) Transcribe(c *echo.Context) error {
	file, header, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the recording as file").Wrap(err)
	}
	defer file.Close()
	text, err := h.Voice.Transcribe(c.Request().Context(), file, header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "transcription failed").Wrap(err)
	}
	return c.JSON(http.StatusOK, map[string]string{"text": text})
}
