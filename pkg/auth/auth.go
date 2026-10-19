// Package auth is bearer-token authentication for an HTTP service behind an
// OpenID Connect issuer: it checks the access token every request carries
// (a gateway in front signs the user in and sends it as Authorization:
// Bearer), keeps the user as a Principal in the request's context, enforces
// the control-set header on requests that create documents, and, as the
// service's own OAuth client, exchanges users' tokens for ones that outlive
// them (RFC 8693) and refreshes those. Nothing here knows the application:
// what a principal may do with its control sets is the caller's.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Config is the token issuer and the service's own client there (OIDC_*):
// Keycloak's realm locally.
type Config struct {
	// IssuerURL is every token's iss exactly; its discovery document names
	// the signing keys and the endpoints unless JWKSURL and TokenURL do.
	IssuerURL string `env:"ISSUER_URL, required"`
	// JWKSURL is where the signing keys are when the issuer's own URLs are
	// not reachable from the server (in a container).
	JWKSURL string `env:"JWKS_URL"`
	// Audience must be one of a token's aud.
	Audience string `env:"AUDIENCE, default=agentlite"`
	// ControlsetClaim is the token claim that lists the user's control sets,
	// a JSON array.
	ControlsetClaim string `env:"CONTROLSET_CLAIM, default=controlset_ids"`
	// ClientID and ClientSecret are the service's own confidential client,
	// which exchanges and refreshes tokens at TokenURL: the issuer's token
	// endpoint from discovery when empty, or an STS that stands in for it.
	ClientID     string `env:"CLIENT_ID, default=agentlite"`
	ClientSecret string `env:"CLIENT_SECRET, required"`
	TokenURL     string `env:"TOKEN_URL"`
	// UserInfoTTL is how long the claims fetched from the userinfo endpoint
	// are kept per user (UserInfo).
	UserInfoTTL time.Duration `env:"USERINFO_TTL, default=5m"`
}

// Principal is the user a request is made by.
type Principal struct {
	Subject  string // sub: the user's id at the issuer
	Username string // preferred_username
	// Controlsets are the token's: what the user may read and write.
	Controlsets []string
	// Controlset is the request's, from the control-set header (Controlset
	// middleware); "" on a request that names none.
	Controlset string
	// Roles are the token's realm roles as they are, and its client roles as
	// client:role; Groups the groups claim, when the token carries one.
	Roles, Groups []string
	Token         string // the access token itself
	Expiry        time.Time
}

// Bearer is the principal's token as an Authorization header; "" without one.
func (p Principal) Bearer() string {
	if p.Token == "" {
		return ""
	}
	return "Bearer " + p.Token
}

type principalKey struct{}

// With returns ctx made by p.
func With(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// From returns the principal ctx was made by; nil outside a request.
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// ErrUnauthorized is a request without a valid token.
var ErrUnauthorized = errors.New("unauthorized")

// Verifier checks access tokens with the issuer's keys.
type Verifier struct {
	v        *oidc.IDTokenVerifier
	provider *oidc.Provider // nil without discovery (JWKSURL set)
	claim    string
	userinfo userinfoCache
}

// New builds the verifier once: the issuer's signing keys are fetched with
// ctx, which must outlive the server, and cached.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	check := &oidc.Config{ClientID: cfg.Audience} // aud must contain it
	v := &Verifier{claim: cfg.ControlsetClaim, userinfo: userinfoCache{ttl: cfg.UserInfoTTL}}
	if cfg.JWKSURL != "" {
		v.v = oidc.NewVerifier(cfg.IssuerURL, oidc.NewRemoteKeySet(ctx, cfg.JWKSURL), check)
		return v, nil
	}
	p, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	v.v, v.provider = p.Verifier(check), p
	return v, nil
}

// Verify checks a raw access token: its signature, issuer, expiry and
// audience, that it is an access token (typ Bearer), and that it names its
// user. A token without the control-set claim is valid and holds none.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Principal, error) {
	t, err := v.v.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	var claims struct {
		Typ            string                              `json:"typ"`
		Username       string                              `json:"preferred_username"`
		RealmAccess    struct{ Roles []string }            `json:"realm_access"`
		ResourceAccess map[string]struct{ Roles []string } `json:"resource_access"`
		Groups         []string                            `json:"groups"`
	}
	var all map[string]json.RawMessage // the control-set claim is named by configuration
	if err := errors.Join(t.Claims(&claims), t.Claims(&all)); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrUnauthorized, err)
	}
	p := &Principal{Subject: t.Subject, Username: claims.Username, Token: raw, Expiry: t.Expiry}
	switch err := json.Unmarshal(all[v.claim], &p.Controlsets); {
	case all[v.claim] != nil && err != nil:
		return nil, fmt.Errorf("%w: %s is not an array: %w", ErrUnauthorized, v.claim, err)
	case claims.Typ != "Bearer":
		return nil, fmt.Errorf("%w: not an access token (typ %q)", ErrUnauthorized, claims.Typ)
	case t.Subject == "" || claims.Username == "":
		return nil, fmt.Errorf("%w: no sub or preferred_username", ErrUnauthorized)
	}
	p.Groups, p.Roles = claims.Groups, claims.RealmAccess.Roles
	for _, client := range slices.Sorted(maps.Keys(claims.ResourceAccess)) {
		for _, r := range claims.ResourceAccess[client].Roles {
			p.Roles = append(p.Roles, client+":"+r)
		}
	}
	return p, nil
}
