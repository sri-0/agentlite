package handler

import (
	"context"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v5"
)

// ThreadSources lists the sources of a thread's conversation, by number.
func (h *Handler) ThreadSources(c *echo.Context) error {
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	list, err := h.Store.Sources(ctx, rootID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, newList(list))
}

// ThreadSource returns one source of a thread's conversation: what a
// [source:N] citation in the thread refers to.
func (h *Handler) ThreadSource(c *echo.Context) error {
	n, err := strconv.Atoi(c.Param("n"))
	if err != nil || n < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "a source number is a positive integer")
	}
	ctx := c.Request().Context()
	rootID, err := h.rootOf(ctx, c.Param("id"))
	if err != nil {
		return err
	}
	s, err := h.Store.Source(ctx, rootID, n)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, s)
}

// rootOf is the conversation a thread belongs to: sources and artifacts are
// kept per conversation, so a sub-agent's thread sees its parent's.
func (h *Handler) rootOf(ctx context.Context, threadID string) (string, error) {
	t, err := h.Store.Thread(ctx, threadID)
	if err != nil {
		return "", err
	}
	return t.RootID, nil
}
