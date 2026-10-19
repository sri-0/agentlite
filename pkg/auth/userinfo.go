package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// userinfoCache keeps the userinfo claims fetched per subject for ttl.
type userinfoCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]userinfoEntry
}

type userinfoEntry struct {
	claims  map[string]any
	expires time.Time
}

// UserInfo returns the claims the issuer's userinfo endpoint holds for the
// principal, fetched with their token and kept for Config.UserInfoTTL: for
// what a gated feature needs that the access token does not carry. It needs
// discovery (no JWKSURL).
func (v *Verifier) UserInfo(ctx context.Context, p *Principal) (map[string]any, error) {
	if v.provider == nil {
		return nil, errors.New("auth: userinfo needs the issuer's discovery document (OIDC_JWKS_URL is set)")
	}
	c := &v.userinfo
	c.mu.Lock()
	e, ok := c.m[p.Subject]
	c.mu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.claims, nil
	}
	info, err := v.provider.UserInfo(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.Token}))
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := info.Claims(&claims); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]userinfoEntry{}
	}
	c.m[p.Subject] = userinfoEntry{claims: claims, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return claims, nil
}
