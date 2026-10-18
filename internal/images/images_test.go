package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"agentlite/config"
	"agentlite/internal/providers"
	"agentlite/pkg/modelcatalog"
	"agentlite/pkg/transport"
)

// request is what the fake image server was sent.
type request struct {
	path   string
	json   map[string]any    // a JSON body
	fields map[string]string // a multipart body's fields
	file   []byte            // and its image
	name   string            // the image's file name
	ctype  string            // and content type
}

// newStudio returns a studio whose image model is served by a fake that
// records the request and answers with the image "made".
func newStudio(t *testing.T, edits bool) (*Studio, *request) {
	t.Helper()
	got := &request{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		if r.Header.Get("Content-Type") == "application/json" {
			_ = json.NewDecoder(r.Body).Decode(&got.json)
		} else {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("multipart: %v", err)
			}
			got.fields = map[string]string{}
			for k, v := range r.MultipartForm.Value {
				got.fields[k] = v[0]
			}
			if f, h, err := r.FormFile("image"); err == nil {
				got.file, _ = io.ReadAll(f)
				got.name, got.ctype = h.Filename, h.Header.Get("Content-Type")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("made"))}}})
	}))
	t.Cleanup(srv.Close)
	registry, err := providers.NewRegistry(&config.ModelsConfig{Providers: []config.Provider{{
		ID: "img", BaseURL: srv.URL + "/v1", Models: []modelcatalog.Model{{ID: "qwen/qwen-image", Type: modelcatalog.TypeImage}},
	}}}, t.TempDir(), transport.Env{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return New(registry, nil, nil, "img/qwen/qwen-image", edits), got
}

// TestGenerate: a new image goes to /images/generations; an edit goes there
// with OpenRouter's input_references, or with edits on to /images/edits as
// a multipart upload of the original.
func TestGenerate(t *testing.T) {
	original := &reference{body: []byte("original"), mediaType: "image/png"}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original.body)
	for _, tc := range []struct {
		name  string
		edits bool
		ref   *reference
		check func(t *testing.T, got *request)
	}{
		{name: "new", ref: nil, check: func(t *testing.T, got *request) {
			if got.path != "/v1/images/generations" || got.json["prompt"] != "a kite" || got.json["model"] != "qwen/qwen-image" || got.json["input_references"] != nil {
				t.Errorf("got %s %v", got.path, got.json)
			}
		}},
		{name: "edit on generations", ref: original, check: func(t *testing.T, got *request) {
			refs, _ := got.json["input_references"].([]any)
			url := ""
			if len(refs) == 1 {
				url, _ = refs[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
			}
			if got.path != "/v1/images/generations" || url != dataURL || got.json["prompt"] != "a kite" {
				t.Errorf("got %s %v", got.path, got.json)
			}
		}},
		{name: "edit on edits", edits: true, ref: original, check: func(t *testing.T, got *request) {
			if got.path != "/v1/images/edits" || got.fields["prompt"] != "a kite" || got.fields["model"] != "qwen/qwen-image" {
				t.Errorf("got %s %v", got.path, got.fields)
			}
			if string(got.file) != "original" || got.name != "image.png" || got.ctype != "image/png" {
				t.Errorf("image %q, name %q, type %q", got.file, got.name, got.ctype)
			}
		}},
		{name: "new with edits", edits: true, ref: nil, check: func(t *testing.T, got *request) {
			if got.path != "/v1/images/generations" {
				t.Errorf("got %s", got.path)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, got := newStudio(t, tc.edits)
			img, err := s.generate(context.Background(), s.model, "a kite", tc.ref)
			if err != nil || string(img) != "made" {
				t.Fatalf("image %q, err %v", img, err)
			}
			tc.check(t, got)
		})
	}
}
