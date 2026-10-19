package auth

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
)

// Middleware lets in only requests with a valid access token, except the
// paths in open, and puts the principal in their context. The rest get 401.
func (v *Verifier) Middleware(open ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			if slices.Contains(open, r.URL.Path) {
				return next(c)
			}
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				return unauthorized(c, errors.New("no bearer token"))
			}
			p, err := v.Verify(r.Context(), raw)
			if err != nil {
				return unauthorized(c, err)
			}
			c.SetRequest(r.WithContext(With(r.Context(), p)))
			return next(c)
		}
	}
}

func unauthorized(c *echo.Context, err error) error {
	c.Response().Header().Set("WWW-Authenticate", `Bearer realm="agentlite"`)
	return echo.NewHTTPError(http.StatusUnauthorized, "a valid access token is required").Wrap(err)
}

// ControlsetConfig is how a request names the control set its new documents
// get (AUTH_CONTROLSET_*).
type ControlsetConfig struct {
	// Header carries it.
	Header string `env:"HEADER, default=X-Control-Set"`
	// ExemptPaths are the POST routes that need none: request paths, or
	// route patterns (/mcp/:id).
	ExemptPaths []string `env:"EXEMPT_PATHS"`
}

// Controlset enforces the control-set header on every POST, after
// Middleware: exactly one value, one of the principal's, which becomes
// Principal.Controlset. Anything else is 400, a control set that is not
// theirs 403. The exempt paths, and other methods, pass as they are.
func Controlset(cfg ControlsetConfig) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			p := From(r.Context())
			if r.Method != http.MethodPost || p == nil || slices.Contains(cfg.ExemptPaths, r.URL.Path) || slices.Contains(cfg.ExemptPaths, c.Path()) {
				return next(c)
			}
			values := r.Header.Values(cfg.Header)
			if len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.Contains(values[0], ",") {
				return echo.NewHTTPError(http.StatusBadRequest, "send one control set as "+cfg.Header)
			}
			cs := strings.TrimSpace(values[0])
			if !slices.Contains(p.Controlsets, cs) {
				return echo.NewHTTPError(http.StatusForbidden, "control set "+cs+" is not one of yours")
			}
			p.Controlset = cs
			return next(c)
		}
	}
}

// RequireRole guards a route with a role of the token: a realm role by its
// name, a client role as client:role. 403 without it.
func RequireRole(role string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if p := From(c.Request().Context()); p == nil || !slices.Contains(p.Roles, role) {
				return echo.NewHTTPError(http.StatusForbidden, "the role "+role+" is required")
			}
			return next(c)
		}
	}
}
