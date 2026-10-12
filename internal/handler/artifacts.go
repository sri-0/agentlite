package handler

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"agentlite/internal/artifacts"
)

// ThreadArtifacts lists the artefacts of a thread's conversation, newest
// first, without their bodies.
func (h *Handler) ThreadArtifacts(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	list, err := h.Store.Artifacts(ctx, rootID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(list))
}

// ArtifactContent serves an artefact's body, from S3 or the record.
func (h *Handler) ArtifactContent(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	a, err := h.Store.Artifact(ctx, rootID, c.Param("artifactId"))
	if err != nil {
		return err
	}
	body, err := h.Artifacts.Body(ctx, a)
	if err != nil {
		return err
	}
	defer body.Close()
	// A body is model-written: opened directly, an HTML one must not run as this origin.
	c.Response().Header().Set("Content-Security-Policy", "sandbox")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Stream(http.StatusOK, artifacts.ContentType(a.Kind), body)
}
