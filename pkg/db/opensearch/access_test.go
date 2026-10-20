package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/oauth2"
)

// callerCtx is a caller: a user's own request without sets, a run's with
// its thread's.
func callerCtx(username, controlset string, sets ...string) context.Context {
	return WithCaller(context.Background(), Caller{
		Token:    oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok-" + username}),
		Username: username, Controlsets: sets, Controlset: controlset,
	})
}

// TestConfine: a run's search is ANDed with its control set: around a
// scored query (kept as must), as the only filter of an empty one, and
// inside a k-NN query's own filter, so the nearest are found among the
// allowed documents. A user's own request (no sets) is sent as it is: DLS
// governs it.
func TestConfine(t *testing.T) {
	only := map[string]any{"terms": map[string]any{"controlset_id": []string{"C1000"}}}
	term := map[string]any{"term": map[string]any{"root_id": "r"}}
	for _, tc := range []struct {
		name string
		sets []string
		body map[string]any
		want map[string]any
	}{
		{"query", []string{"C1000"}, map[string]any{"query": term},
			map[string]any{"query": map[string]any{"bool": map[string]any{"must": term, "filter": []any{only}}}}},
		{"no query", []string{"C1000"}, map[string]any{"size": 1},
			map[string]any{"size": 1, "query": map[string]any{"bool": map[string]any{"filter": []any{only}}}}},
		{"knn", []string{"C1000"}, map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5}}}},
			map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5, "filter": only}}}}},
		{"knn with filter", []string{"C1000"}, map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5, "filter": term}}}},
			map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5,
				"filter": map[string]any{"bool": map[string]any{"filter": []any{term, only}}}}}}}},
		{"a user's own request", nil, map[string]any{"query": term}, map[string]any{"query": term}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confine(tc.body, tc.sets)
			if d := cmp.Diff(tc.want, tc.body); d != "" {
				t.Fatalf("(-want +got)\n%s", d)
			}
		})
	}
}

// TestStamp: a new document gets the caller's control set for new documents
// and is created by them, now; every write is updated by them, now; a
// document's creation stands. Inside a run, a label outside its control set
// is refused; a new document with no control set, and a write with no
// caller, are refused.
func TestStamp(t *testing.T) {
	ctx := callerCtx("alice", "C1000")
	start := time.Now()
	var m Marks
	if err := stamp(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if m.ControlsetID != "C1000" || m.UserCreatedBy != "alice" || m.UserUpdatedBy != "alice" || m.CreatedAt.Before(start) || m.UpdatedAt != m.CreatedAt {
		t.Fatalf("new document: %+v", m)
	}

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m = Marks{ControlsetID: "C1001", UserCreatedBy: "bob", UserUpdatedBy: "bob", CreatedAt: created, UpdatedAt: created}
	if err := stamp(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if m.ControlsetID != "C1001" || m.UserCreatedBy != "bob" || m.UserUpdatedBy != "alice" || !m.CreatedAt.Equal(created) || !m.UpdatedAt.After(created) {
		t.Fatalf("changed document: %+v", m)
	}

	// A run is confined to its thread's control set, even for a user who
	// holds more.
	run := callerCtx("carol", "C1000", "C1000")
	if err := stamp(run, &Marks{ControlsetID: "C1001"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("label outside the run's control set: got %v, want ErrForbidden", err)
	}
	if err := stamp(callerCtx("alice", ""), &Marks{}); !errors.Is(err, ErrNoControlset) {
		t.Fatalf("no control set: got %v, want ErrNoControlset", err)
	}
	if err := stamp(context.Background(), &Marks{ControlsetID: "C1000"}); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("no caller: got %v, want ErrNoCaller", err)
	}
}

// TestCheckOwner: only a document's creator may change it.
func TestCheckOwner(t *testing.T) {
	ctx := callerCtx("alice", "")
	if err := CheckOwner(ctx, Marks{UserCreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Marks{{UserCreatedBy: "bob"}, {}} {
		if err := CheckOwner(ctx, m); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%+v: got %v, want ErrForbidden", m, err)
		}
	}
	if err := CheckOwner(context.Background(), Marks{UserCreatedBy: "alice"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no caller: got %v, want ErrForbidden", err)
	}
}

// TestBearer: every request carries its caller's token, replacing any
// Authorization already there; one without a caller is never sent.
func TestBearer(t *testing.T) {
	r, _ := http.NewRequestWithContext(callerCtx("alice", ""), http.MethodGet, "http://os/x", nil)
	r.SetBasicAuth("admin", "admin")
	if err := (bearer{}).SignRequest(r); err != nil {
		t.Fatal(err)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer tok-alice" {
		t.Fatalf("Authorization = %q", got)
	}
	r, _ = http.NewRequest(http.MethodGet, "http://os/x", nil)
	if err := (bearer{}).SignRequest(r); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("no caller: got %v, want ErrNoCaller", err)
	}
}

// TestNoCallerFailsClosed: the store refuses to search or write without a
// caller before anything is sent, and a URL with credentials.
func TestNoCallerFailsClosed(t *testing.T) {
	c, err := New(Config{URL: "http://127.0.0.1:1"}) // nothing listens there
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Threads(context.Background()); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("search: got %v, want ErrNoCaller", err)
	}
	if err := c.SaveThread(context.Background(), &Thread{ID: "t"}); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("write: got %v, want ErrNoCaller", err)
	}
	if _, err := c.Thread(context.Background(), "t"); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("get: got %v, want ErrNoCaller", err)
	}
	if _, err := New(Config{URL: "http://admin:secret@localhost:9200"}); err == nil {
		t.Fatal("a URL with credentials was accepted")
	}
}

// TestMarksJSON: the marks are top-level fields of each document, as the
// index mappings and the DLS query name them, and a document has each
// timestamp once.
func TestMarksJSON(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	raw, _ := json.Marshal(Thread{ID: "t", Marks: Marks{ControlsetID: "C1000", UserCreatedBy: "a", UserUpdatedBy: "b", CreatedAt: at, UpdatedAt: at}})
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for k, v := range map[string]string{"controlset_id": "C1000", "user_created_by": "a", "user_updated_by": "b", "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z"} {
		if got[k] != v {
			t.Fatalf("%s = %v in %s", k, got[k], raw)
		}
	}
}
