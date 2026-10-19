package auth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"agentlite/pkg/logging"
)

// Client is the service's own confidential client at the token endpoint
// (Config.ClientID, ClientSecret, TokenURL): it exchanges users' tokens for
// refresh tokens on their session (RFC 8693) and refreshes those, through
// x/oauth2, authenticated with HTTP Basic.
type Client struct {
	oauth oauth2.Config
	http  *http.Client
	group singleflight.Group // one refresh per key at a time (Source)
}

// NewClient builds the client; its token endpoint is cfg's, or the one the
// issuer's discovery document names.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.TokenURL == "" {
		p, err := oidc.NewProvider(ctx, cfg.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		cfg.TokenURL = p.Endpoint().TokenURL
	}
	return &Client{
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{TokenURL: cfg.TokenURL, AuthStyle: oauth2.AuthStyleInHeader}},
		http: &http.Client{Transport: withHeader{http.DefaultTransport}, Timeout: 30 * time.Second},
	}, nil
}

// ErrRefused is a token that cannot be had: the endpoint refused the exchange
// or the refresh (the subject token is expired or revoked, the session it was
// on has ended), or none is kept to refresh.
var ErrRefused = errors.New("token refused")

// Exchange trades a subject token, a user's access token, for a refresh
// token and the access token that comes with it (RFC 8693,
// requested_token_type refresh_token). header goes on the request: what the
// token is for, for an STS that narrows it. Keycloak's standard exchange
// returns a refresh token on the user's session.
func (c *Client) Exchange(ctx context.Context, subject string, header http.Header) (Token, error) {
	ctx = context.WithValue(context.WithValue(ctx, oauth2.HTTPClient, c.http), headerKey{}, header)
	got, err := c.oauth.Exchange(ctx, "", // Exchange sends an empty code=, which token exchange ignores
		oauth2.SetAuthURLParam("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange"),
		oauth2.SetAuthURLParam("subject_token", subject),
		oauth2.SetAuthURLParam("subject_token_type", "urn:ietf:params:oauth:token-type:access_token"),
		oauth2.SetAuthURLParam("requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token"))
	if err := refused(err, "token exchange"); err != nil {
		return Token{}, err
	}
	if got.RefreshToken == "" {
		return Token{}, errors.New("token exchange: no refresh token was issued")
	}
	return token(got, time.Time{}), nil
}

// Refresh renews a token with its refresh token; the new refresh token, when
// one comes, takes the old one's place.
func (c *Client) Refresh(ctx context.Context, refresh string) (Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	got, err := c.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err := refused(err, "token refresh"); err != nil {
		return Token{}, err
	}
	return token(got, time.Time{}), nil
}

// refused maps an endpoint's refusal (400, 401) to ErrRefused.
func refused(err error, what string) error {
	if e, ok := errors.AsType[*oauth2.RetrieveError](err); ok && (e.Response.StatusCode == http.StatusBadRequest || e.Response.StatusCode == http.StatusUnauthorized) {
		return fmt.Errorf("%w: %s: %s %s", ErrRefused, what, e.ErrorCode, e.ErrorDescription)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// token is an endpoint's response as a Token: refresh_expires_in, when it
// says (Keycloak does), is the refresh token's lifetime; else refreshExpiry.
func token(t *oauth2.Token, refreshExpiry time.Time) Token {
	tok := Token{Access: t.AccessToken, AccessExpiry: t.Expiry, Refresh: t.RefreshToken, RefreshExpiry: refreshExpiry}
	if s, ok := t.Extra("refresh_expires_in").(float64); ok && s > 0 {
		tok.RefreshExpiry = time.Now().Add(time.Duration(s) * time.Second)
	}
	return tok
}

type headerKey struct{}

// withHeader is the client's transport: it adds the header the request's
// context carries (Exchange).
type withHeader struct{ http.RoundTripper }

func (t withHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	if h, ok := r.Context().Value(headerKey{}).(http.Header); ok && len(h) > 0 {
		r = r.Clone(r.Context())
		maps.Copy(r.Header, h)
	}
	return t.RoundTripper.RoundTrip(r)
}

// Source is the token kept under key in cache as an oauth2.TokenSource: its
// access token, renewed with its refresh token early before it expires, one
// renewal per key at a time. A renewal the endpoint refuses drops the token;
// that, and no token kept, is ErrRefused. Renewals are audit entries on log.
func (c *Client) Source(cache Cache, key string, early time.Duration, log zerolog.Logger) oauth2.TokenSource {
	return source{c: c, cache: cache, key: key, early: early, log: log}
}

type source struct {
	c     *Client
	cache Cache
	key   string
	early time.Duration
	log   zerolog.Logger
}

func (s source) Token() (*oauth2.Token, error) {
	if kept, ok := s.cache.Get(s.key); ok && time.Until(kept.AccessExpiry) > s.early {
		return access(kept), nil
	}
	v, err, _ := s.c.group.Do(s.key, func() (any, error) { return s.renew() })
	if err != nil {
		return nil, err
	}
	return v.(*oauth2.Token), nil
}

func (s source) renew() (*oauth2.Token, error) {
	kept, ok := s.cache.Get(s.key)
	switch {
	case ok && time.Until(kept.AccessExpiry) > s.early: // renewed while this call waited
		return access(kept), nil
	case !ok:
		return nil, fmt.Errorf("%w: the token is gone", ErrRefused)
	}
	got, err := s.c.Refresh(context.Background(), kept.Refresh)
	if errors.Is(err, ErrRefused) {
		s.cache.Delete(s.key)
		logging.Audit(s.log).Err(err).Msg("token dropped: its refresh was refused")
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if got.RefreshExpiry.IsZero() {
		got.RefreshExpiry = kept.RefreshExpiry
	}
	s.cache.Put(s.key, got)
	logging.Audit(s.log).Time("expiry", got.AccessExpiry).Time("refresh_expiry", got.RefreshExpiry).Msg("token refreshed")
	return access(got), nil
}

func access(t Token) *oauth2.Token {
	return &oauth2.Token{AccessToken: t.Access, TokenType: "Bearer", Expiry: t.AccessExpiry}
}
