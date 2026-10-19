package tokens

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// issuer is a fake token endpoint: it exchanges the subject "good" and
// refreshes the refresh tokens it gave out, unless dead.
type issuer struct {
	t                *testing.T
	srv              *httptest.Server
	exchanges, grant atomic.Int32
	expiresIn        int // the access tokens' lifetime, seconds
	dead             atomic.Bool
	mu               sync.Mutex
	forms            []map[string]string
}

func newIssuer(t *testing.T) *issuer {
	is := &issuer{t: t, expiresIn: 900}
	is.srv = httptest.NewServer(http.HandlerFunc(is.token))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) token(w http.ResponseWriter, r *http.Request) {
	id, secret, _ := r.BasicAuth()
	_ = r.ParseForm()
	form := map[string]string{"client": id + ":" + secret}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	is.mu.Lock()
	is.forms = append(is.forms, form)
	is.mu.Unlock()
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
	}
	var n int32
	switch form["grant_type"] {
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		if form["subject_token"] != "good" {
			refuse("invalid_request")
			return
		}
		n = is.exchanges.Add(1)
	case "refresh_token":
		if is.dead.Load() {
			refuse("invalid_grant")
			return
		}
		n = 100 + is.grant.Add(1)
	default:
		refuse("unsupported_grant_type")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("at-%d", n), "expires_in": is.expiresIn, "token_type": "Bearer",
		"refresh_token": fmt.Sprintf("rt-%d", n), "refresh_expires_in": 1800,
	})
}

func newTokens(t *testing.T, is *issuer, param string) *Tokens {
	t.Helper()
	tk, err := New(t.Context(), Config{ClientID: "agentlite", ClientSecret: "s3cret", TokenURL: is.srv.URL, ControlSetParam: param}, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// TestExchange: the user's token is exchanged once for a refresh token on
// their session (RFC 8693, as the server's own client) and kept per
// conversation and user; a token that cannot be exchanged is ErrSignIn.
func TestExchange(t *testing.T) {
	is := newIssuer(t)
	tk := newTokens(t, is, "")
	src, err := tk.Source(t.Context(), "root", "C1000", "alice", "good")
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := src.Token(); err != nil || tok.AccessToken != "at-1" {
		t.Fatalf("got %v, %v", tok, err)
	}
	want := map[string]string{
		"client": "agentlite:s3cret", "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "subject_token": "good",
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token", "requested_token_type": "urn:ietf:params:oauth:token-type:refresh_token",
	}
	if got := is.forms[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exchange form %v, want %v", got, want)
	}
	// Kept: a later run of the same user in the conversation needs no subject.
	if _, err := tk.Source(t.Context(), "root", "C1000", "alice", ""); err != nil || is.exchanges.Load() != 1 {
		t.Fatalf("kept token: %v, %d exchanges", err, is.exchanges.Load())
	}
	// Per user and conversation: another user, or conversation, mints its own.
	if _, err := tk.Source(t.Context(), "root", "C1000", "bob", ""); !errors.Is(err, ErrSignIn) {
		t.Fatalf("bob without a subject: got %v, want ErrSignIn", err)
	}
	if _, err := tk.Source(t.Context(), "other", "C1000", "alice", "expired"); !errors.Is(err, ErrSignIn) {
		t.Fatalf("refused exchange: got %v, want ErrSignIn", err)
	}
}

// TestControlSetParam: an exchange can name the thread's control set, for an
// STS that narrows the token to it.
func TestControlSetParam(t *testing.T) {
	is := newIssuer(t)
	if _, err := newTokens(t, is, "controlset_id").Source(t.Context(), "root", "C1000", "alice", "good"); err != nil {
		t.Fatal(err)
	}
	if got := is.forms[0]["controlset_id"]; got != "C1000" {
		t.Fatalf("controlset_id = %q", got)
	}
}

// TestRefresh: an access token near its expiry is renewed with the refresh
// token, as the same client, without the user's token; a refresh that is
// refused (the user's session ended) is ErrSignIn, and the token is dropped
// so the user's next turn mints a new one.
func TestRefresh(t *testing.T) {
	is := newIssuer(t)
	is.expiresIn = 10 // inside earlyExpiry: every Token call refreshes
	tk := newTokens(t, is, "")
	src, err := tk.Source(t.Context(), "root", "C1000", "alice", "good")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token()
	if err != nil || tok.AccessToken != "at-101" {
		t.Fatalf("got %v, %v; want the refreshed at-101", tok, err)
	}
	if f := is.forms[1]; f["grant_type"] != "refresh_token" || f["refresh_token"] != "rt-1" || f["client"] != "agentlite:s3cret" {
		t.Fatalf("refresh form %v", f)
	}
	if tok, _ := src.Token(); tok.AccessToken != "at-102" || is.forms[2]["refresh_token"] != "rt-101" {
		t.Fatalf("the newest refresh token is used: got %v with %v", tok, is.forms[2])
	}

	is.dead.Store(true)
	if _, err := src.Token(); !errors.Is(err, ErrSignIn) {
		t.Fatalf("dead session: got %v, want ErrSignIn", err)
	}
	if _, err := tk.Source(t.Context(), "root", "C1000", "alice", ""); !errors.Is(err, ErrSignIn) {
		t.Fatalf("a dropped token is not kept: got %v", err)
	}
}

// TestReuse: a valid access token is reused, and concurrent runs share one
// exchange and one refresh.
func TestReuse(t *testing.T) {
	is := newIssuer(t)
	tk := newTokens(t, is, "")
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			src, err := tk.Source(t.Context(), "root", "C1000", "alice", "good")
			if err == nil {
				_, err = src.Token()
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := is.exchanges.Load(); n != 1 {
		t.Fatalf("%d exchanges, want 1", n)
	}
	if n := is.grant.Load(); n != 0 {
		t.Fatalf("%d refreshes of a valid token, want 0", n)
	}
	// Near its expiry, many callers refresh it once.
	k := key("root", "alice")
	kept, _ := tk.cache.Get(k)
	kept.AccessExpiry = time.Now()
	tk.cache.Put(k, kept)
	src := source{t: tk, key: k}
	for range 20 {
		wg.Go(func() {
			if _, err := src.Token(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := is.grant.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
}

// TestMemory: putting a token drops the expired ones.
func TestMemory(t *testing.T) {
	c := newMemory()
	c.Put("old", Token{RefreshExpiry: time.Now().Add(-time.Second)})
	c.Put("new", Token{RefreshExpiry: time.Now().Add(time.Hour)})
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired token kept")
	}
	if _, ok := c.Get("new"); !ok {
		t.Fatal("live token dropped")
	}
}
