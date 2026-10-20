// Package server builds the Echo application: middleware and routes.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"

	"agentlite/config"
	"agentlite/internal/handler"
	"agentlite/internal/handler/wellknown"
	"agentlite/internal/passthrough"
	"agentlite/internal/runs"
	"agentlite/internal/skills"
	"agentlite/internal/sts"
	"agentlite/pkg/auth"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/logging"
	"agentlite/pkg/s3"
)

// New wires middleware and routes. Echo logs through slog, which
// pkg/logging routes to zerolog. Its request logger records each request's
// real status and id; Recover runs inside it, so a panic is logged, with its
// stack, as a 500. Every route but /health needs an access token (the
// socket's upgrade too); CORS answers a preflight before that check. Every
// POST but AUTH_CONTROLSET_EXEMPT_PATHS needs X-Control-Set as well
// (auth.Controlset), the pass-through and the MCP proxy included; then the
// request's OpenSearch calls are made as its user (caller).
func New(cfg *config.Config, h *handler.Handler) *echo.Echo {
	e := echo.New()
	e.Logger = slog.Default()
	e.HTTPErrorHandler = errorHandler(echo.DefaultHTTPErrorHandler(false), h.Log)
	e.Use(middleware.RequestID(), middleware.RequestLogger(), middleware.Recover(),
		middleware.BodyLimit(cfg.MaxBodyMB<<20),
		middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: cfg.AllowedOrigins,
			AllowHeaders: []string{"Content-Type", "Authorization", cfg.Controlsets.Header},
		}),
		h.Auth.Middleware("/health"), auth.Controlset(cfg.Controlsets), caller)

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
	e.GET("/v1/sources", h.RecentSources)

	// Next-prompt suggestions, from a thread, what's being typed, or both (internal/suggest).
	e.POST("/v1/suggestions", h.Suggestions)

	// Workspaces: the folders threads are filed under.
	e.GET("/v1/workspaces", h.Workspaces)
	e.POST("/v1/workspaces", h.CreateWorkspace)
	workspace := e.Group("/v1/workspaces/:id", uuidID)
	workspace.PATCH("", h.UpdateWorkspace)
	workspace.DELETE("", h.DeleteWorkspace)

	// The skills library.
	e.GET("/v1/skills", h.ListSkills)
	e.POST("/v1/skills", h.CreateSkill, middleware.BodyLimit(skills.MaxBytes+1<<20)) // the zip, and the form around it
	skill := e.Group("/v1/skills/:id", uuidID)
	skill.GET("", h.GetSkill)
	skill.PATCH("", h.UpdateSkill)
	skill.DELETE("", h.DeleteSkill)
	skill.GET("/files/*", h.SkillFile, sandboxed)
	skill.PUT("/files/*", h.WriteSkillFile, middleware.BodyLimit(skills.MaxBytes))

	// Files users upload for RAG, which a message names with #id (internal/rag/userfiles).
	e.GET("/v1/rag/files", h.Files.ListFiles)
	e.POST("/v1/rag/files", h.Files.PostFile, middleware.BodyLimit(h.Files.MaxBytes()+1<<20)) // the file, and the form around it
	file := e.Group("/v1/rag/files/:id", uuidID)
	file.GET("", h.Files.GetFile)
	file.PATCH("", h.Files.PatchFile)
	file.GET("/content", h.Files.FileContent, sandboxed)
	file.DELETE("", h.Files.DeleteFile)

	// What opencode and npx skills set themselves up from (docs/OPENCODE.md).
	wk := &wellknown.Handler{Config: cfg, Registry: h.Registry, Skills: h.Skills}
	e.GET("/.well-known/opencode", wk.Opencode)
	e.GET("/.well-known/agent-skills/index.json", wk.SkillsIndex)
	e.GET("/.well-known/agent-skills/:name/*", wk.SkillFile, sandboxed)
	e.Any("/mcp/:id", h.MCPProxy)

	// The voice prompt: a recording in, its text out (internal/voice).
	if v := cfg.Models.Voice; v != nil {
		e.POST("/v1/voice/transcriptions", h.Transcribe, middleware.BodyLimit(int64(v.MaxMB+1)<<20)) // the recording, and the form around it
	}

	// Agent chat: a new thread, then the socket.
	e.POST("/v1/agent/chat", h.StartThread)
	e.GET("/v1/agent/chat", h.Chat)

	// Pass-through to the providers (/v1/chat/completions and the rest).
	passthrough.Register(e, h.Registry)
	return e
}

// caller makes the request's OpenSearch calls as its user (pkg/auth's
// principal): their own token, labelling new documents with the control set
// the request named. Document-level security alone governs what they read;
// a run is confined further (runs.Manager).
func caller(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		if p := auth.From(r.Context()); p != nil {
			c.SetRequest(r.WithContext(opensearch.WithCaller(r.Context(), opensearch.Caller{
				Token:    oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.Token, Expiry: p.Expiry}),
				Username: p.Username, Controlset: p.Controlset,
			})))
		}
		return next(c)
	}
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
		Address:         net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), // IPv6 hosts too
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
// handlers can return them as they are, and records every refusal (a
// control set that is not the user's, a change to someone else's document,
// a label a run may not write) as an audit entry.
func errorHandler(next echo.HTTPErrorHandler, log zerolog.Logger) echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		for _, m := range []struct {
			err    error
			status int
		}{
			{runs.ErrInvalid, http.StatusBadRequest},
			{opensearch.ErrNoControlset, http.StatusBadRequest},
			{skills.ErrInvalid, http.StatusBadRequest},
			{opensearch.ErrUnknownWorkspace, http.StatusBadRequest},
			{runs.ErrNotFound, http.StatusNotFound},
			{opensearch.ErrNotFound, http.StatusNotFound},
			{skills.ErrNotFound, http.StatusNotFound},
			{skills.ErrFileNotFound, http.StatusNotFound},
			{s3.ErrNotFound, http.StatusNotFound},
			{runs.ErrBusy, http.StatusConflict},
			{sts.ErrSignIn, http.StatusUnauthorized},
			{opensearch.ErrForbidden, http.StatusForbidden},
		} {
			if errors.Is(err, m.err) {
				err = echo.NewHTTPError(m.status, err.Error())
				break
			}
		}
		// Echo's request logger and the server each call this once; the
		// second call finds the response written.
		if he, ok := errors.AsType[*echo.HTTPError](err); ok && he.Code == http.StatusForbidden && !committed(c) {
			r := c.Request()
			e := logging.Audit(log).Err(err).Str("method", r.Method).Str("path", r.URL.Path)
			if p := auth.From(r.Context()); p != nil {
				e = e.Str("user_id", p.Subject).Str("username", p.Username).Str("controlset_id", p.Controlset)
			}
			e.Msg("request refused")
		}
		next(c, err)
	}
}

func committed(c *echo.Context) bool {
	r, _ := echo.UnwrapResponse(c.Response())
	return r != nil && r.Committed
}
