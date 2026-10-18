package files

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"
)

// PostFile is POST /v1/files: a multipart form with the file and, to file it
// under a workspace, its workspaceId. The reply is the file's record.
func (s *Store) PostFile(c *echo.Context) error {
	upload, header, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the file as file").Wrap(err)
	}
	defer upload.Close()
	if header.Size > MaxBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "a file is at most 25 MiB")
	}
	body, err := io.ReadAll(upload)
	if err != nil {
		return err
	}
	f, err := s.Add(c.Request().Context(), header.Filename, c.FormValue("workspaceId"), body)
	switch {
	case errors.Is(err, ErrInvalid):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrUnsupported):
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, ErrEmbed):
		msg := ErrEmbed.Error() // not err's text: it names the provider's URL
		if refused, ok := errors.AsType[*openai.Error](err); ok && refused.Message != "" {
			msg = "the embedding model refused the file: " + refused.Message
		}
		return echo.NewHTTPError(http.StatusBadGateway, msg).Wrap(err)
	case err != nil:
		return err
	}
	return c.JSON(http.StatusCreated, f)
}

// ListFiles is GET /v1/files, newest first: every file, or with ?workspace=
// one workspace's, "none" for the unfiled ones.
func (s *Store) ListFiles(c *echo.Context) error {
	var workspaceID *string
	if w := c.QueryParam("workspace"); w != "" {
		if w == "none" {
			w = ""
		}
		workspaceID = &w
	}
	found, err := s.records.Files(c.Request().Context(), workspaceID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"object": "list", "data": found})
}

// GetFile is GET /v1/files/:id, a file's record.
func (s *Store) GetFile(c *echo.Context) error {
	f, err := s.records.File(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, f)
}

// FileContent is GET /v1/files/:id/content, the file itself, with ranges, so
// audio and video can seek (and Safari plays video at all).
func (s *Store) FileContent(c *echo.Context) error {
	ctx := c.Request().Context()
	f, err := s.records.File(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	body, err := s.Body(ctx, f)
	if err != nil {
		return err
	}
	defer body.Close()
	w := c.Response()
	w.Header().Set("Content-Type", f.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": f.Name}))
	http.ServeContent(w, c.Request(), f.Name, f.CreatedAt, body)
	return nil
}

// DeleteFile is DELETE /v1/files/:id.
func (s *Store) DeleteFile(c *echo.Context) error {
	if err := s.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
