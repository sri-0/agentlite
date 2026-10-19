// Package sts keeps thread tokens: the tokens a run calls OpenSearch with,
// in place of the user's own access token, which expires in minutes and may
// not be at hand (a resumed run, a sub-agent, the turn's memory write). A
// thread token is minted for a (conversation, user) pair by exchanging the
// user's access token (pkg/auth: Keycloak's token endpoint today, an STS
// that narrows the token to the thread's control set later; the control set
// goes on the exchange request as a header) and refreshed as long as the
// user's session lasts. Tokens are kept in memory only: a restart loses
// them, and the next turn mints again from the token it comes with
// (docs/AUTH.md).
package sts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"agentlite/pkg/auth"
	"agentlite/pkg/logging"
)

// Config is the thread-token policy (STS_*).
type Config struct {
	// ControlsetHeader is the header of the exchange request that carries
	// the thread's control set, for an STS that narrows the token to it.
	// Keycloak ignores it.
	ControlsetHeader string `env:"CONTROLSET_HEADER, default=X-Control-Set"`
	// EarlyExpiry is how long before its expiry an access token is renewed;
	// MinLife how much of a kept token's refresh lifetime must be left for a
	// run to use it rather than mint again.
	EarlyExpiry time.Duration `env:"EARLY_EXPIRY, default=30s"`
	MinLife     time.Duration `env:"MIN_LIFE, default=5m"`
}

// ErrSignIn is a thread token that cannot be had: the user's token could not
// be exchanged, or the thread token can no longer be refreshed (the user's
// session ended). The user must sign in again; nothing falls back to
// another token.
var ErrSignIn = errors.New("authentication failed")

// STS mints, keeps and refreshes thread tokens.
type STS struct {
	client *auth.Client
	cache  auth.Cache
	cfg    Config
	group  singleflight.Group // one exchange per key at a time
	log    zerolog.Logger
}

// New returns the thread tokens of the client's issuer's users.
func New(client *auth.Client, cfg Config, log zerolog.Logger) *STS {
	return &STS{client: client, cache: auth.NewMemory(), cfg: cfg, log: log}
}

// key is a thread token's: one per conversation and user, so a user's run in
// a conversation someone else shares with them uses their own.
func key(rootID, userID string) string { return rootID + "\x00" + userID }

// Source returns the thread token of the conversation and user, minting one
// from subject (the user's access token) when none is kept, or the one kept
// is near its end. Without a subject only a kept one will do. controlset is
// the conversation's, sent on the exchange.
func (s *STS) Source(ctx context.Context, rootID, controlset, userID, subject string) (oauth2.TokenSource, error) {
	k := key(rootID, userID)
	log := s.log.With().Str("user_id", userID).Str("root_id", rootID).Str("controlset_id", controlset).Logger()
	if kept, ok := s.cache.Get(k); ok && time.Until(kept.RefreshExpiry) > s.cfg.MinLife {
		return source{s.client.Source(s.cache, k, s.cfg.EarlyExpiry, log)}, nil
	}
	if subject == "" {
		logging.Audit(log).Msg("thread token refused: none is kept and the request carries no token")
		return nil, fmt.Errorf("%w: no thread token is kept for this conversation", ErrSignIn)
	}
	_, err, _ := s.group.Do(k, func() (any, error) {
		header := http.Header{}
		header.Set(s.cfg.ControlsetHeader, controlset)
		tok, err := s.client.Exchange(ctx, subject, header)
		if err == nil {
			s.cache.Put(k, tok)
			logging.Audit(log).Time("refresh_expiry", tok.RefreshExpiry).Msg("thread token minted")
		}
		return nil, err
	})
	switch {
	case errors.Is(err, auth.ErrRefused):
		logging.Audit(log).Err(err).Msg("thread token refused")
		return nil, fmt.Errorf("%w: %w", ErrSignIn, err)
	case err != nil:
		return nil, err
	}
	return source{s.client.Source(s.cache, k, s.cfg.EarlyExpiry, log)}, nil
}

// source is a thread token as a run holds it: one that can no longer be
// refreshed is ErrSignIn.
type source struct{ oauth2.TokenSource }

func (s source) Token() (*oauth2.Token, error) {
	t, err := s.TokenSource.Token()
	if errors.Is(err, auth.ErrRefused) {
		return nil, fmt.Errorf("%w: %w", ErrSignIn, err)
	}
	return t, err
}
