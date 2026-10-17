// Package server builds the Echo application: middleware and routes.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/rs/zerolog"

	"agentlite/internal/config"
	"agentlite/internal/handler"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// New wires middleware and routes.
func New(cfg *config.Config, h *handler.Handler, log zerolog.Logger) *echo.Echo {
	e := echo.New()
	e.Logger = slog.Default()
	e.HTTPErrorHandler = errorHandler(echo.DefaultHTTPErrorHandler(false))
	e.Use(middleware.RequestID())
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(cfg.MaxBodyMB << 20))
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: cfg.AllowedOrigins,
		AllowHeaders: []string{"Content-Type", "Authorization"},
	}))
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogMethod: true, LogURI: true, LogLatency: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			ev := log.Info()
			if v.Error != nil {
				ev = log.Error().Err(v.Error)
			}
			ev.Str("method", v.Method).Str("uri", v.URI).Int("status", v.Status).Dur("latency", v.Latency).Msg("request")
			return nil
		},
	}))

	e.GET("/health", h.Health)
	e.GET("/v1/models", h.Models)
	e.GET("/v1/agents", h.Agents)
	e.GET("/v1/connectors", h.Connectors)
	e.GET("/v1/connectors/:id", h.Connector)
	e.GET("/v1/threads", h.Threads)
	e.GET("/v1/threads/:id", h.Thread)
	e.PATCH("/v1/threads/:id", h.UpdateThread)
	e.DELETE("/v1/threads/:id", h.DeleteThread)
	e.GET("/v1/threads/:id/messages", h.Messages)
	e.GET("/v1/threads/:id/interrupts", h.Interrupts)
	e.GET("/v1/threads/:id/sources", h.ThreadSources)
	e.GET("/v1/threads/:id/sources/:n", h.ThreadSource)
	e.GET("/v1/threads/:id/artifacts", h.ThreadArtifacts)
	e.GET("/v1/artifacts", h.RecentArtifacts)
	e.GET("/v1/threads/:id/artifacts/:artifactId/content", h.ArtifactContent, sandboxed)
	if h.Skills != nil {
		e.GET("/v1/skills", h.ListSkills)
		e.POST("/v1/skills", h.CreateSkill, middleware.BodyLimit(skills.MaxBytes+1<<20)) // the zip, and the form around it
		e.GET("/v1/skills/:id", h.GetSkill)
		e.PATCH("/v1/skills/:id", h.UpdateSkill)
		e.DELETE("/v1/skills/:id", h.DeleteSkill)
		e.GET("/v1/skills/:id/files/*", h.SkillFile, sandboxed)
		e.PUT("/v1/skills/:id/files/*", h.WriteSkillFile, middleware.BodyLimit(skills.MaxBytes))
		// npx skills discovery: the index, and each skill's files by its name next to it.
		e.GET("/.well-known/agent-skills/index.json", h.SkillsIndex)
		e.GET("/.well-known/agent-skills/:name/*", h.SkillsIndexFile, sandboxed)
	}
	e.POST("/v1/agent/chat", h.StartThread)
	e.GET("/v1/agent/chat", h.Chat)
	h.RegisterProxies(e)
	return e
}

// sandboxed serves what users and models wrote (artifact bodies, skill
// files): opened directly in a browser, nothing in it may run as this origin
// or be sniffed into HTML.
var sandboxed = middleware.SecureWithConfig(middleware.SecureConfig{
	ContentSecurityPolicy: "sandbox", ContentTypeNosniff: "nosniff",
})

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, cfg *config.Config, e *echo.Echo) error {
	start := echo.StartConfig{
		Address:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		HideBanner:      true,
		GracefulTimeout: 10 * time.Second,
		BeforeServeFunc: func(s *http.Server) error {
			s.ReadHeaderTimeout = 10 * time.Second
			s.WriteTimeout = 0 // sockets and proxied streams are long-lived
			return nil
		},
	}
	return start.Start(ctx, e)
}

// errorHandler turns the service's sentinel errors into HTTP statuses, so
// handlers can return them as they are.
func errorHandler(next echo.HTTPErrorHandler) echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		for _, m := range []struct {
			err    error
			status int
		}{
			{runs.ErrInvalid, http.StatusBadRequest},
			{skills.ErrInvalid, http.StatusBadRequest},
			{runs.ErrNotFound, http.StatusNotFound},
			{opensearch.ErrNotFound, http.StatusNotFound},
			{skills.ErrNotFound, http.StatusNotFound},
			{skills.ErrFileNotFound, http.StatusNotFound},
			{s3.ErrNotFound, http.StatusNotFound},
			{runs.ErrBusy, http.StatusConflict},
		} {
			if errors.Is(err, m.err) {
				err = echo.NewHTTPError(m.status, err.Error())
				break
			}
		}
		next(c, err)
	}
}
