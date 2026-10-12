package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type ctxKey struct{}

func TestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Header }))
	defer srv.Close()

	client, err := New(Config{
		Headers: map[string]string{"X-Static": "on"},
		ContextHeaders: func(ctx context.Context) http.Header {
			if id, ok := ctx.Value(ctxKey{}).(string); ok {
				return http.Header{"X-Thread-Id": {id}}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "t-1")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	req.Header.Set("X-Static", "caller")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got.Get("X-Static") != "on" || got.Get("X-Thread-ID") != "t-1" {
		t.Errorf("headers sent = %v", got)
	}
	if req.Header.Get("X-Static") != "caller" {
		t.Errorf("caller's request was changed: %v", req.Header)
	}
}

func TestBadTLSFiles(t *testing.T) {
	if _, err := New(Config{TLS: &TLS{CAFile: "/does/not/exist.pem"}}); err == nil {
		t.Error("want an error for a missing CA file")
	}
}
