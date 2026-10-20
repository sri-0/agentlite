package userfiles

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v5"
	"github.com/openai/openai-go/v3"

	"agentlite/pkg/db/opensearch"
)

// fileItem is a file's record as the API serves it: with whether the
// caller uploaded it, and so may change or delete it.
type fileItem struct {
	opensearch.File
	Mine bool `json:"mine"`
}

// PostFile is POST /v1/rag/files: a multipart form with the file and, to file it
// under a workspace, its workspaceId; tags, repeated, tag it; is_shared true
// shares it with everyone who holds its control set (it is the uploader's
// alone otherwise). The reply is the file's record.
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
	shared, err := formBool(c.FormValue("is_shared"))
	if err != nil {
		return err
	}
	f, err := s.Add(c.Request().Context(), header.Filename, c.FormValue("workspaceId"), form.Value["tags"], shared, body)
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
	return c.JSON(http.StatusCreated, fileItem{File: *f, Mine: true})
}

// formBool is a form field's boolean: false when it is not set, 400 when it
// is not one.
func formBool(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, echo.NewHTTPError(http.StatusBadRequest, "is_shared must be true or false")
	}
	return b, nil
}

// ListFiles is GET /v1/rag/files, newest first: every file the caller can
// see (their own, and the shared ones), or with ?workspace= one workspace's,
// "none" for the unfiled ones.
func (s *Store) ListFiles(c *echo.Context) error {
	ctx := c.Request().Context()
	var workspaceID *string
	if w := c.QueryParam("workspace"); w != "" {
		if w == "none" {
			w = ""
		}
		workspaceID = &w
	}
	found, err := s.records.Files(ctx, workspaceID)
	if err != nil {
		return err
	}
	items := make([]fileItem, 0, len(found))
	for _, f := range found {
		items = append(items, fileItem{File: f, Mine: opensearch.Mine(ctx, f.Marks)})
	}
	return c.JSON(http.StatusOK, map[string]any{"object": "list", "data": items})
}

// GetFile is GET /v1/rag/files/:id, a file's record.
func (s *Store) GetFile(c *echo.Context) error {
	ctx := c.Request().Context()
	f, err := s.records.File(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, fileItem{File: *f, Mine: opensearch.Mine(ctx, f.Marks)})
}

// PatchFile is PATCH /v1/rag/files/:id: {is_shared?, tags?}, a field left
// out staying as it is; only the uploader may.
func (s *Store) PatchFile(c *echo.Context) error {
	var body struct {
		IsShared *bool     `json:"is_shared"`
		Tags     *[]string `json:"tags"`
	}
	if err := c.Bind(&body); err != nil {
		return err
	}
	if body.IsShared == nil && body.Tags == nil {
		return echo.NewHTTPError(http.StatusBadRequest, "is_shared or tags is required")
	}
	f, err := s.Update(c.Request().Context(), c.Param("id"), body.Tags, body.IsShared)
	if errors.Is(err, ErrInvalid) {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, fileItem{File: *f, Mine: true})
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
