// Package auth verifies the user's access token on every request. A gateway
// in front of the server signs the user in and sends the token as
// Authorization: Bearer (cmd/devgateway locally); the server only checks it,
// with the issuer's keys. The verified user is a Principal in the request's
// context, and the store's Caller with their own token (pkg/db/opensearch):
// OpenSearch's document-level security limits what they read to the
// control sets the token names (docs/AUTH.md).
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/labstack/echo/v5"
	"golang.org/x/oauth2"

	"agentlite/pkg/db/opensearch"
)

// Config is the token issuer (OIDC_*): Keycloak's realm locally.
type Config struct {
	// IssuerURL is every token's iss exactly; its discovery document names
	// the signing keys unless JWKSURL does.
	IssuerURL string `env:"ISSUER_URL, required"`
	// JWKSURL is where the signing keys are when the issuer's own URLs are
	// not reachable from the server (in a container).
	JWKSURL string `env:"JWKS_URL"`
	// Audience must be one of a token's aud.
	Audience string `env:"AUDIENCE, default=agentlite"`
}

// Principal is the user a request is made by.
type Principal struct {
	Subject     string   // sub: the user's id at the issuer
	Username    string   // preferred_username: what documents are created and updated by
	ControlSets []string // controlset_ids: what they may read and write
	Token       string   // the access token itself
	Expiry      time.Time
}

// Bearer is the principal's token as an Authorization header.
func (p Principal) Bearer() string { return "Bearer " + p.Token }

type principalKey struct{}

// With returns ctx made by p: the principal, and the store's caller with
// p's own token, confined to p's control sets.
func With(ctx context.Context, p *Principal) context.Context {
	ctx = context.WithValue(ctx, principalKey{}, p)
	return opensearch.WithCaller(ctx, opensearch.Caller{
		Token:    oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.Token, Expiry: p.Expiry}),
		Username: p.Username, ControlSets: p.ControlSets,
	})
}

// From returns the principal ctx was made by; nil outside a request.
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// ErrUnauthorized is a request without a valid token.
var ErrUnauthorized = errors.New("unauthorized")

// Verifier checks access tokens.
type Verifier struct {
	v *oidc.IDTokenVerifier
}

// New builds the verifier once: the issuer's signing keys are fetched with
// ctx, which must outlive the server, and cached.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	check := &oidc.Config{ClientID: cfg.Audience} // aud must contain it
	if cfg.JWKSURL != "" {
		return &Verifier{v: oidc.NewVerifier(cfg.IssuerURL, oidc.NewRemoteKeySet(ctx, cfg.JWKSURL), check)}, nil
	}
	p, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return &Verifier{v: p.Verifier(check)}, nil
}

// Verify checks a raw access token: its signature, issuer, expiry and
// audience, that it is an access token (typ Bearer), and that it names its
// user. A token without controlset_ids is valid and sees nothing.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Principal, error) {
	t, err := v.v.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	var claims struct {
		Typ         string   `json:"typ"`
		Username    string   `json:"preferred_username"`
		ControlSets []string `json:"controlset_ids"`
	}
	switch err := t.Claims(&claims); {
	case err != nil:
		return nil, fmt.Errorf("%w: claims: %w", ErrUnauthorized, err)
	case claims.Typ != "Bearer":
		return nil, fmt.Errorf("%w: not an access token (typ %q)", ErrUnauthorized, claims.Typ)
	case t.Subject == "" || claims.Username == "":
		return nil, fmt.Errorf("%w: no sub or preferred_username", ErrUnauthorized)
	}
	return &Principal{Subject: t.Subject, Username: claims.Username, ControlSets: claims.ControlSets, Token: raw, Expiry: t.Expiry}, nil
}

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

// HeaderControlSet names the control set a request's new documents get.
const HeaderControlSet = "X-Control-Set"

// ControlSet guards a route that creates documents (a thread, a workspace, a
// file, a skill): the request names exactly one control set, one of the
// principal's, which the documents it creates get. Anything else is 400, a
// control set that is not theirs 403.
func ControlSet(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		values := r.Header.Values(HeaderControlSet)
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.Contains(values[0], ",") {
			return echo.NewHTTPError(http.StatusBadRequest, "send one control set as "+HeaderControlSet)
		}
		cs := strings.TrimSpace(values[0])
		p := From(r.Context())
		if p == nil || !slices.Contains(p.ControlSets, cs) {
			return echo.NewHTTPError(http.StatusForbidden, "control set "+cs+" is not one of yours")
		}
		caller, _ := opensearch.CallerFrom(r.Context())
		caller.ControlSet = cs
		c.SetRequest(r.WithContext(opensearch.WithCaller(r.Context(), caller)))
		return next(c)
	}
}
