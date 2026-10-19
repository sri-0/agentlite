package sts

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

	"agentlite/pkg/auth"
)

// endpoint is a fake token endpoint: it exchanges the subject "good" and
// refreshes what it gave out, recording each request's control-set header.
type endpoint struct {
	srv       *httptest.Server
	exchanges atomic.Int32
	mu        sync.Mutex
	headers   []string
}

func newEndpoint(t *testing.T) *endpoint {
	ep := &endpoint{}
	ep.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ep.mu.Lock()
		ep.headers = append(ep.headers, r.Header.Get("X-Thread-Control-Set"))
		ep.mu.Unlock()
		if r.PostForm.Get("grant_type") != "refresh_token" {
			if r.PostForm.Get("subject_token") != "good" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":"invalid_request","error_description":"Invalid token"}`)
				return
			}
			ep.exchanges.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "expires_in": 900, "token_type": "Bearer", "refresh_token": "rt", "refresh_expires_in": 1800,
		})
	}))
	t.Cleanup(ep.srv.Close)
	return ep
}

func newSTS(t *testing.T, ep *endpoint) *STS {
	t.Helper()
	client, err := auth.NewClient(t.Context(), auth.Config{ClientID: "agentlite", ClientSecret: "s3cret", TokenURL: ep.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return New(client, Config{ControlsetHeader: "X-Thread-Control-Set", EarlyExpiry: 30 * time.Second, MinLife: 5 * time.Minute}, zerolog.Nop())
}

// TestSource: a thread token is minted once per conversation and user, with
// the thread's control set on the exchange request, and kept: a later run
// of the same user needs no subject, another user's does. One that cannot
// be had is ErrSignIn.
func TestSource(t *testing.T) {
	ep := newEndpoint(t)
	s := newSTS(t, ep)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			src, err := s.Source(t.Context(), "root", "C1000", "alice", "good")
			if err == nil {
				_, err = src.Token()
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := ep.exchanges.Load(); n != 1 || ep.headers[0] != "C1000" {
		t.Fatalf("%d exchanges, header %q; want 1, C1000", n, ep.headers[0])
	}
	if _, err := s.Source(t.Context(), "root", "C1000", "alice", ""); err != nil || ep.exchanges.Load() != 1 {
		t.Fatalf("kept token: %v, %d exchanges", err, ep.exchanges.Load())
	}
	if _, err := s.Source(t.Context(), "root", "C1000", "bob", ""); !errors.Is(err, ErrSignIn) {
		t.Fatalf("bob without a subject: got %v, want ErrSignIn", err)
	}
	if _, err := s.Source(t.Context(), "other", "C1000", "alice", "expired"); !errors.Is(err, ErrSignIn) || !errors.Is(err, auth.ErrRefused) {
		t.Fatalf("refused exchange: got %v, want ErrSignIn", err)
	}
	// A kept token near its end is minted again rather than used.
	kept, _ := s.cache.Get(key("root", "alice"))
	kept.RefreshExpiry = time.Now().Add(time.Minute)
	s.cache.Put(key("root", "alice"), kept)
	if _, err := s.Source(t.Context(), "root", "C1000", "alice", "good"); err != nil || ep.exchanges.Load() != 2 {
		t.Fatalf("near its end: %v, %d exchanges; want a new one", err, ep.exchanges.Load())
	}
	kept, _ = s.cache.Get(key("root", "alice"))
	kept.RefreshExpiry = time.Now().Add(time.Minute)
	s.cache.Put(key("root", "alice"), kept)
	if _, err := s.Source(t.Context(), "root", "C1000", "alice", ""); !errors.Is(err, ErrSignIn) {
		t.Fatalf("near its end without a subject: got %v, want ErrSignIn", err)
	}
}
