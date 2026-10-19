// Package server builds the Echo application: middleware and routes.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/rs/zerolog"

	"agentlite/config"
	"agentlite/internal/handler"
	"agentlite/internal/handler/wellknown"
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

	// Health, models, agents and connectors.
	e.GET("/health", h.Health)
	e.GET("/v1/models", h.Models)
	e.GET("/v1/agents", h.Agents)
	e.GET("/v1/connectors", h.Connectors)
	e.GET("/v1/connectors/:id", h.Connector)

	// Threads and what each conversation recorded.
	e.GET("/v1/threads", h.Threads)
	thread := e.Group("/v1/threads/:id", uuidID)
	thread.GET("", h.Thread)
	thread.PATCH("", h.UpdateThread)
	thread.DELETE("", h.DeleteThread)
	thread.GET("/messages", h.Messages)
	thread.GET("/interrupts", h.Interrupts)
	thread.GET("/sources", h.ThreadSources)
	thread.GET("/sources/:n", h.ThreadSource)
	thread.GET("/artifacts", h.ThreadArtifacts)
	thread.GET("/artifacts/:artifactId/content", h.ArtifactContent, sandboxed)
	e.GET("/v1/artifacts", h.RecentArtifacts)

	// The skills library.
	e.GET("/v1/skills", h.ListSkills)
	e.POST("/v1/skills", h.CreateSkill, middleware.BodyLimit(skills.MaxBytes+1<<20)) // the zip, and the form around it
	skill := e.Group("/v1/skills/:id", uuidID)
	skill.GET("", h.GetSkill)
	skill.PATCH("", h.UpdateSkill)
	skill.DELETE("", h.DeleteSkill)
	skill.GET("/files/*", h.SkillFile, sandboxed)
	skill.PUT("/files/*", h.WriteSkillFile, middleware.BodyLimit(skills.MaxBytes))

	// What opencode and npx skills set themselves up from (docs/OPENCODE.md).
	wk := &wellknown.Handler{Config: cfg, Registry: h.Registry, Skills: h.Skills}
	e.GET("/.well-known/opencode", wk.Opencode)
	e.GET("/.well-known/agent-skills/index.json", wk.SkillsIndex)
	e.GET("/.well-known/agent-skills/:name/*", wk.SkillFile, sandboxed)
	e.Any("/mcp/:id", h.MCPProxy)

	// Agent chat: a new thread, then the socket.
	e.POST("/v1/agent/chat", h.StartThread)
	e.GET("/v1/agent/chat", h.Chat)

	// Pass-through to the providers (/v1/chat/completions and the rest).
	h.RegisterProxies(e)
	return e
}

// uuidID refuses an :id that is not a UUIDv7, the ids the server makes,
// before any handler sees it.
func uuidID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if id, err := uuid.Parse(c.Param("id")); err != nil || id.Version() != 7 {
			return echo.NewHTTPError(http.StatusBadRequest, "id must be a UUIDv7")
		}
		return next(c)
	}
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
			// Shutdown does not close hijacked connections; ending their
			// requests' context closes the sockets as going away.
			base, cancel := context.WithCancel(context.Background())
			s.BaseContext = func(net.Listener) context.Context { return base }
			s.RegisterOnShutdown(cancel)
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
