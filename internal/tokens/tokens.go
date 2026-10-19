// Package tokens keeps thread tokens: the tokens a run calls OpenSearch with,
// in place of the user's own access token, which expires in minutes and may
// not be at hand (a resumed run, a sub-agent, the turn's memory write). A
// thread token is minted for a (conversation, user) pair by an RFC 8693 token
// exchange of the user's access token at the token endpoint (Keycloak's
// today; an STS that narrows the token to the thread's control set later),
// and refreshed as long as the user's session lasts. Tokens are kept in
// memory only: a restart loses them, and the next turn mints again from the
// token it comes with (docs/AUTH.md).
package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// Config is the server's own client at the token endpoint (OIDC_*).
type Config struct {
	ClientID     string `env:"CLIENT_ID, default=agentlite"`
	ClientSecret string `env:"CLIENT_SECRET, required"`
	// TokenURL is where tokens are exchanged and refreshed; "" is the
	// issuer's, from its discovery document. An STS can take its place.
	TokenURL string `env:"TOKEN_URL"`
	// ControlSetParam, when set, names a form parameter of the exchange that
	// carries the thread's control set, for an STS that narrows the token to
	// it. Keycloak takes none.
	ControlSetParam string `env:"EXCHANGE_CONTROLSET_PARAM"`
}

// ErrSignIn is a thread token that cannot be had: the user's token could not
// be exchanged, or the thread token can no longer be refreshed (the user's
// session ended). The user must sign in again; nothing falls back to
// another token.
var ErrSignIn = errors.New("sign in again")

// Lifetimes: an access token is renewed this long before it expires, and a
// thread token whose refresh token has less left than minLife is minted
// again rather than used.
const (
	earlyExpiry = 30 * time.Second
	minLife     = 5 * time.Minute
)

// Token is a thread token as the cache keeps it: the access token runs send
// and the refresh token that renews it, with when each expires.
type Token struct {
	Access        string
	AccessExpiry  time.Time
	Refresh       string
	RefreshExpiry time.Time
}

// Cache keeps thread tokens by key. Memory is the one there is; a cache that
// survives restarts (Valkey with AES-GCM, Vault) would implement the same.
type Cache interface {
	Get(key string) (Token, bool)
	Put(key string, t Token)
	Delete(key string)
}

// Tokens mints, keeps and refreshes thread tokens.
type Tokens struct {
	oauth  oauth2.Config
	param  string
	client *http.Client
	cache  Cache
	group  singleflight.Group // one exchange or refresh per key at a time
	log    zerolog.Logger
}

// New returns the thread tokens of the issuer's users; its token endpoint is
// cfg's, or the one its discovery document names.
func New(ctx context.Context, cfg Config, issuer string, log zerolog.Logger) (*Tokens, error) {
	if cfg.TokenURL == "" {
		p, err := oidc.NewProvider(ctx, issuer)
		if err != nil {
			return nil, fmt.Errorf("tokens: %w", err)
		}
		cfg.TokenURL = p.Endpoint().TokenURL
	}
	return &Tokens{
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{TokenURL: cfg.TokenURL, AuthStyle: oauth2.AuthStyleInHeader}},
		param: cfg.ControlSetParam, client: &http.Client{Timeout: 30 * time.Second}, cache: newMemory(), log: log,
	}, nil
}

// key is a thread token's: one per conversation and user, so a user's run in
// a conversation someone else shares with them uses their own.
func key(rootID, userID string) string { return rootID + "\x00" + userID }

// Source returns the thread token of the conversation and user, minting one
// from subject (the user's access token) when none is kept, or the one kept
// is near its end. Without a subject only a kept one will do. controlSet is
// the conversation's, for an exchange that narrows to it.
func (t *Tokens) Source(ctx context.Context, rootID, controlSet, userID, subject string) (oauth2.TokenSource, error) {
	k := key(rootID, userID)
	if kept, ok := t.cache.Get(k); ok && time.Until(kept.RefreshExpiry) > minLife {
		return source{t: t, key: k}, nil
	}
	if subject == "" {
		return nil, fmt.Errorf("%w: no thread token is kept for this conversation", ErrSignIn)
	}
	_, err, _ := t.group.Do("mint\x00"+k, func() (any, error) {
		tok, err := t.exchange(ctx, controlSet, subject)
		if err == nil {
			t.cache.Put(k, tok)
			t.log.Debug().Str("root", rootID).Str("user", userID).Time("refresh_expiry", tok.RefreshExpiry).Msg("thread token minted")
		}
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	return source{t: t, key: k}, nil
}

// exchange trades the user's access token for a refresh token on their
// session (RFC 8693; Keycloak's standard token exchange, with "allow
// refresh token" on the same session) and the access token that comes with
// it.
func (t *Tokens) exchange(ctx context.Context, controlSet, subject string) (Token, error) {
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {subject},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:refresh_token"},
	}
	if t.param != "" {
		form.Set(t.param, controlSet)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.oauth.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(t.oauth.ClientID), url.QueryEscape(t.oauth.ClientSecret))
	resp, err := t.client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("token exchange: %w", err)
	}
	var got struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int    `json:"expires_in"`
		RefreshToken     string `json:"refresh_token"`
		RefreshExpiresIn int    `json:"refresh_expires_in"`
		Error            string `json:"error"`
		Description      string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &got)
	switch {
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		// The user's token is expired or revoked, or not theirs to exchange.
		return Token{}, fmt.Errorf("%w: token exchange refused: %s %s", ErrSignIn, got.Error, got.Description)
	case resp.StatusCode != http.StatusOK || got.AccessToken == "" || got.RefreshToken == "":
		return Token{}, fmt.Errorf("token exchange: %s: %s %s", resp.Status, got.Error, got.Description)
	}
	now := time.Now()
	return Token{
		Access: got.AccessToken, AccessExpiry: now.Add(time.Duration(got.ExpiresIn) * time.Second),
		Refresh: got.RefreshToken, RefreshExpiry: now.Add(time.Duration(got.RefreshExpiresIn) * time.Second),
	}, nil
}

// source is a kept thread token as an oauth2.TokenSource: its access token,
// renewed with its refresh token shortly before it expires.
type source struct {
	t   *Tokens
	key string
}

func (s source) Token() (*oauth2.Token, error) {
	if kept, ok := s.t.cache.Get(s.key); ok && time.Until(kept.AccessExpiry) > earlyExpiry {
		return &oauth2.Token{AccessToken: kept.Access, TokenType: "Bearer", Expiry: kept.AccessExpiry}, nil
	}
	v, err, _ := s.t.group.Do("refresh\x00"+s.key, func() (any, error) { return s.t.refresh(s.key) })
	if err != nil {
		return nil, err
	}
	return v.(*oauth2.Token), nil
}

// refresh renews a kept thread token. One that cannot be renewed is dropped,
// so the user's next turn mints a new one.
func (t *Tokens) refresh(k string) (*oauth2.Token, error) {
	kept, ok := t.cache.Get(k)
	if ok && time.Until(kept.AccessExpiry) > earlyExpiry { // renewed while this call waited
		return &oauth2.Token{AccessToken: kept.Access, TokenType: "Bearer", Expiry: kept.AccessExpiry}, nil
	}
	if !ok {
		return nil, fmt.Errorf("%w: the thread token is gone", ErrSignIn)
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, t.client)
	got, err := t.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: kept.Refresh}).Token()
	if _, refused := errors.AsType[*oauth2.RetrieveError](err); refused {
		t.cache.Delete(k)
		t.log.Warn().Err(err).Msg("thread token dropped: its refresh was refused")
		return nil, fmt.Errorf("%w: the thread token could not be refreshed: %w", ErrSignIn, err)
	}
	if err != nil {
		return nil, fmt.Errorf("refresh thread token: %w", err)
	}
	// Each refresh returns a new refresh token (oauth2 keeps the old one if
	// not) and moves its expiry on.
	kept = Token{Access: got.AccessToken, AccessExpiry: got.Expiry, Refresh: got.RefreshToken, RefreshExpiry: kept.RefreshExpiry}
	if s, ok := got.Extra("refresh_expires_in").(float64); ok {
		kept.RefreshExpiry = time.Now().Add(time.Duration(s) * time.Second)
	}
	t.cache.Put(k, kept)
	t.log.Debug().Time("expiry", kept.AccessExpiry).Time("refresh_expiry", kept.RefreshExpiry).Msg("thread token refreshed")
	return got, nil
}

// memory is the in-process cache. Putting a token drops the ones whose
// refresh token has expired, so it holds no more than the live ones.
type memory struct {
	mu sync.Mutex
	m  map[string]Token
}

func newMemory() *memory { return &memory{m: map[string]Token{}} }

func (c *memory) Get(key string) (Token, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[key]
	return t, ok
}

func (c *memory) Put(key string, t Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, old := range c.m {
		if old.RefreshExpiry.Before(now) {
			delete(c.m, k)
		}
	}
	c.m[key] = t
}

func (c *memory) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}
