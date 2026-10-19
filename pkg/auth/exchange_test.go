package auth

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

// endpoint is a fake token endpoint: it exchanges the subject "good" and
// refreshes the refresh tokens it gave out, unless dead.
type endpoint struct {
	srv              *httptest.Server
	exchanges, grant atomic.Int32
	expiresIn        int // the access tokens' lifetime, seconds
	dead             atomic.Bool
	mu               sync.Mutex
	forms            []map[string]string // each request's form, its client and its X-Control-Set
}

func newEndpoint(t *testing.T) *endpoint {
	ep := &endpoint{expiresIn: 900}
	ep.srv = httptest.NewServer(http.HandlerFunc(ep.token))
	t.Cleanup(ep.srv.Close)
	return ep
}

func (ep *endpoint) token(w http.ResponseWriter, r *http.Request) {
	id, secret, _ := r.BasicAuth()
	_ = r.ParseForm()
	form := map[string]string{"client": id + ":" + secret, "X-Control-Set": r.Header.Get("X-Control-Set")}
	for k := range r.PostForm {
		form[k] = r.PostForm.Get(k)
	}
	ep.mu.Lock()
	ep.forms = append(ep.forms, form)
	ep.mu.Unlock()
	refuse := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":"nope"}`, code)
	}
	var n int32
	switch form["grant_type"] {
	case "urn:ietf:params:oauth:grant-type:token-exchange":
		if form["subject_token"] != "good" {
			refuse("invalid_request")
			return
		}
		n = ep.exchanges.Add(1)
	case "refresh_token":
		if ep.dead.Load() {
			refuse("invalid_grant")
			return
		}
		n = 100 + ep.grant.Add(1)
	default:
		refuse("unsupported_grant_type")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("at-%d", n), "expires_in": ep.expiresIn, "token_type": "Bearer",
		"refresh_token": fmt.Sprintf("rt-%d", n), "refresh_expires_in": 1800,
	})
}

func newClient(t *testing.T, ep *endpoint) *Client {
	t.Helper()
	c, err := NewClient(t.Context(), Config{ClientID: "agentlite", ClientSecret: "s3cret", TokenURL: ep.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestExchange: a subject token is exchanged for a refresh token (RFC 8693,
// as the service's own client, with the header given); one that cannot be
// is ErrRefused.
func TestExchange(t *testing.T) {
	ep := newEndpoint(t)
	c := newClient(t, ep)
	tok, err := c.Exchange(t.Context(), "good", http.Header{"X-Control-Set": {"C1000"}})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "at-1" || tok.Refresh != "rt-1" || time.Until(tok.AccessExpiry) < 800*time.Second || time.Until(tok.RefreshExpiry) < 1700*time.Second {
		t.Fatalf("got %+v", tok)
	}
	want := map[string]string{
		"client": "agentlite:s3cret", "X-Control-Set": "C1000", "code": "",
		"grant_type": "urn:ietf:params:oauth:grant-type:token-exchange", "subject_token": "good",
		"subject_token_type": "urn:ietf:params:oauth:token-type:access_token", "requested_token_type": "urn:ietf:params:oauth:token-type:refresh_token",
	}
	if got := ep.forms[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exchange form %v, want %v", got, want)
	}
	if _, err := c.Exchange(t.Context(), "expired", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused exchange: got %v, want ErrRefused", err)
	}
}

// TestSource: a kept token's access token is reused while it lasts, renewed
// with its refresh token, as the same client, when it is near its expiry
// (the newest refresh token each time), and concurrent callers share one
// renewal. A refresh that is refused (the session ended) drops the token.
func TestSource(t *testing.T) {
	ep := newEndpoint(t)
	c := newClient(t, ep)
	cache := NewMemory()
	tok, _ := c.Exchange(t.Context(), "good", nil)
	cache.Put("k", tok)
	src := c.Source(cache, "k", 30*time.Second, zerolog.Nop())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got, err := src.Token(); err != nil || got.AccessToken != "at-1" {
				t.Errorf("got %v, %v", got, err)
			}
		})
	}
	wg.Wait()
	if n := ep.grant.Load(); n != 0 {
		t.Fatalf("%d refreshes of a valid token, want 0", n)
	}

	tok.AccessExpiry = time.Now()
	cache.Put("k", tok)
	for range 20 {
		wg.Go(func() {
			if got, err := src.Token(); err != nil || got.AccessToken != "at-101" {
				t.Errorf("got %v, %v; want the refreshed at-101", got, err)
			}
		})
	}
	wg.Wait()
	if n := ep.grant.Load(); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	if f := ep.forms[1]; f["grant_type"] != "refresh_token" || f["refresh_token"] != "rt-1" || f["client"] != "agentlite:s3cret" {
		t.Fatalf("refresh form %v", f)
	}
	kept, _ := cache.Get("k")
	kept.AccessExpiry = time.Now()
	cache.Put("k", kept)
	if got, _ := src.Token(); got.AccessToken != "at-102" || ep.forms[2]["refresh_token"] != "rt-101" {
		t.Fatalf("the newest refresh token is used: got %v with %v", got, ep.forms[2])
	}

	ep.dead.Store(true)
	kept, _ = cache.Get("k")
	kept.AccessExpiry = time.Now()
	cache.Put("k", kept)
	if _, err := src.Token(); !errors.Is(err, ErrRefused) {
		t.Fatalf("dead session: got %v, want ErrRefused", err)
	}
	if _, ok := cache.Get("k"); ok {
		t.Fatal("a refused token was kept")
	}
	if _, err := src.Token(); !errors.Is(err, ErrRefused) {
		t.Fatalf("gone: got %v, want ErrRefused", err)
	}
}

// TestMemory: putting a token drops the expired ones.
func TestMemory(t *testing.T) {
	c := NewMemory()
	c.Put("old", Token{RefreshExpiry: time.Now().Add(-time.Second)})
	c.Put("new", Token{RefreshExpiry: time.Now().Add(time.Hour)})
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired token kept")
	}
	if _, ok := c.Get("new"); !ok {
		t.Fatal("live token dropped")
	}
}
