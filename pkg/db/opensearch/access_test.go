package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/oauth2"
)

func callerCtx(username, controlSet string, sets ...string) context.Context {
	return WithCaller(context.Background(), Caller{
		Token:    oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "tok-" + username}),
		Username: username, ControlSets: sets, ControlSet: controlSet,
	})
}

// TestConfine: every search is ANDed with the caller's control sets: around
// a scored query (kept as must), as the only filter of an empty one, and
// inside a k-NN query's own filter, so the nearest are found among the
// allowed documents.
func TestConfine(t *testing.T) {
	only := map[string]any{"terms": map[string]any{"controlset_id": []string{"C1000"}}}
	term := map[string]any{"term": map[string]any{"root_id": "r"}}
	for _, tc := range []struct {
		name string
		body map[string]any
		want map[string]any
	}{
		{"query", map[string]any{"query": term},
			map[string]any{"query": map[string]any{"bool": map[string]any{"must": term, "filter": []any{only}}}}},
		{"no query", map[string]any{"size": 1},
			map[string]any{"size": 1, "query": map[string]any{"bool": map[string]any{"filter": []any{only}}}}},
		{"knn", map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5}}}},
			map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5, "filter": only}}}}},
		{"knn with filter", map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5, "filter": term}}}},
			map[string]any{"query": map[string]any{"knn": map[string]any{"embedding": map[string]any{"k": 5,
				"filter": map[string]any{"bool": map[string]any{"filter": []any{term, only}}}}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confine(tc.body, []string{"C1000"})
			if d := cmp.Diff(tc.want, tc.body); d != "" {
				t.Fatalf("(-want +got)\n%s", d)
			}
		})
	}
}

// TestStamp: a new document gets the caller's control set for new documents
// and is created by them; every write is updated by them; a label outside
// the caller's control sets, a new document with no control set, and a
// write with no caller are refused.
func TestStamp(t *testing.T) {
	ctx := callerCtx("alice", "C1000", "C1000", "C1001")

	var l Labels
	if err := stamp(ctx, &l); err != nil {
		t.Fatal(err)
	}
	if want := (Labels{ControlSetID: "C1000", UserCreatedBy: "alice", UserUpdatedBy: "alice"}); l != want {
		t.Fatalf("new document: got %+v, want %+v", l, want)
	}

	l = Labels{ControlSetID: "C1001", UserCreatedBy: "bob", UserUpdatedBy: "bob"}
	if err := stamp(ctx, &l); err != nil {
		t.Fatal(err)
	}
	if want := (Labels{ControlSetID: "C1001", UserCreatedBy: "bob", UserUpdatedBy: "alice"}); l != want {
		t.Fatalf("changed document: got %+v, want %+v", l, want)
	}

	l = Labels{ControlSetID: "C1015"}
	if err := stamp(ctx, &l); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign label: got %v, want ErrForbidden", err)
	}
	// A run is confined to its thread's control set, even for a user who
	// holds more.
	run := callerCtx("carol", "C1000", "C1000")
	if err := stamp(run, &Labels{ControlSetID: "C1001"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("label outside the run's control set: got %v, want ErrForbidden", err)
	}
	if err := stamp(callerCtx("alice", "", "C1000"), &Labels{}); !errors.Is(err, ErrNoControlSet) {
		t.Fatalf("no control set: got %v, want ErrNoControlSet", err)
	}
	if err := stamp(context.Background(), &Labels{ControlSetID: "C1000"}); !errors.Is(err, ErrNoCaller) {
		t.Fatalf("no caller: got %v, want ErrNoCaller", err)
	}
}

// TestCheckOwner: only a document's creator may change it.
func TestCheckOwner(t *testing.T) {
	ctx := callerCtx("alice", "", "C1000")
	if err := CheckOwner(ctx, Labels{UserCreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []Labels{{UserCreatedBy: "bob"}, {}} {
		if err := CheckOwner(ctx, l); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%+v: got %v, want ErrForbidden", l, err)
		}
	}
	if err := CheckOwner(context.Background(), Labels{UserCreatedBy: "alice"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no caller: got %v, want ErrForbidden", err)
	}
}

// TestBearer: every request carries its caller's token, replacing any
// Authorization already there; one without a caller is never sent.
func TestBearer(t *testing.T) {
	r, _ := http.NewRequestWithContext(callerCtx("alice", "", "C1000"), http.MethodGet, "http://os/x", nil)
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
// caller before anything is sent, and a caller with no control set finds
// nothing.
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
	found, err := c.Threads(callerCtx("nobody", ""))
	if err != nil || len(found) != 0 {
		t.Fatalf("no control sets: got %v, %v; want nothing", found, err)
	}
	if _, err := New(Config{URL: "http://admin:secret@localhost:9200"}); err == nil {
		t.Fatal("a URL with credentials was accepted")
	}
}

// TestLabelsJSON: the labels are top-level fields of each document, as the
// index mappings and the DLS query name them.
func TestLabelsJSON(t *testing.T) {
	raw, _ := json.Marshal(Thread{ID: "t", Labels: Labels{ControlSetID: "C1000", UserCreatedBy: "a", UserUpdatedBy: "b"}})
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for k, v := range map[string]string{"controlset_id": "C1000", "user_created_by": "a", "user_updated_by": "b"} {
		if got[k] != v {
			t.Fatalf("%s = %v in %s", k, got[k], raw)
		}
	}
}
