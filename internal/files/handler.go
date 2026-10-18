package files

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/pkg/db/opensearch"
)

// PostFile is POST /v1/files: a multipart form with the file and, to file it
// under a workspace, its workspaceId. The reply is the file's record.
func (s *Store) PostFile(c *echo.Context) error {
	header, err := c.FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the file as file").Wrap(err)
	case header.Size > MaxBytes:
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "a file is at most 25 MiB")
	}
	upload, err := header.Open()
	if err != nil {
		return err
	}
	defer upload.Close()
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
	if found == nil {
		found = []opensearch.File{}
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

// FileContent is GET /v1/files/:id/content, the file itself.
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
	c.Response().Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": f.Name}))
	return c.Stream(http.StatusOK, f.MediaType, body)
}

// DeleteFile is DELETE /v1/files/:id.
func (s *Store) DeleteFile(c *echo.Context) error {
	if err := s.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
