package userfiles

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/labstack/echo/v5"
)

// PostFile is POST /v1/rag/files: a multipart form with the file and, to file it
// under a workspace, its workspaceId; tags, repeated, tag it. The reply is the
// file's record.
func (s *Store) PostFile(c *echo.Context) error {
	upload, header, err := c.Request().FormFile("file")
	switch {
	case errors.Is(err, echo.ErrStatusRequestEntityTooLarge):
		return err
	case err != nil:
		return echo.NewHTTPError(http.StatusBadRequest, "send a multipart form with the file as file").Wrap(err)
	}
	defer upload.Close()
	if header.Size > s.MaxBytes() {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("a file is at most %d MiB", s.limits.MaxUploadMB))
	}
	body, err := io.ReadAll(upload)
	if err != nil {
		return err
	}
	form := c.Request().MultipartForm // parsed by FormFile
	f, err := s.Add(c.Request().Context(), header.Filename, c.FormValue("workspaceId"), form.Value["tags"], body)
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

// ListFiles is GET /v1/rag/files, newest first: every file, or with ?workspace=
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

// GetFile is GET /v1/rag/files/:id, a file's record.
func (s *Store) GetFile(c *echo.Context) error {
	f, err := s.records.File(c.Request().Context(), c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, f)
}

// FileContent is GET /v1/rag/files/:id/content, the file itself, with ranges. It
// is always a download: a browser never opens an uploaded file as a page. An
// <img> ignores Content-Disposition, so the Files page's thumbnails still show.
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
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	http.ServeContent(w, c.Request(), f.Name, f.CreatedAt, body)
	return nil
}

// DeleteFile is DELETE /v1/rag/files/:id.
func (s *Store) DeleteFile(c *echo.Context) error {
	if err := s.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
