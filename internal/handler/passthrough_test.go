package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/textproto"
	"testing"
)

// TestJSONModel: the model is read and set; the other fields stay.
func TestJSONModel(t *testing.T) {
	model, withModel := jsonModel([]byte(`{"model":"vllm/Qwen/Qwen-Image-2.1","prompt":"a kite","n":1}`))
	if model != "vllm/Qwen/Qwen-Image-2.1" {
		t.Fatalf("model %q", model)
	}
	var got map[string]any
	if err := json.Unmarshal(withModel("Qwen/Qwen-Image-2.1"), &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "Qwen/Qwen-Image-2.1" || got["prompt"] != "a kite" || got["n"] != 1.0 {
		t.Errorf("got %v", got)
	}
	if model, _ := jsonModel([]byte(`{"prompt":"a kite"}`)); model != "" {
		t.Errorf("no model: %q", model)
	}
}

// TestFormModel: an image edit's form gets the upstream model; its prompt and
// image pass as they came, under the request's boundary.
func TestFormModel(t *testing.T) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("prompt", "make it blue")
	_ = w.WriteField("model", "vllm/Qwen/Qwen-Image-Edit-2511")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="image"; filename="kite.png"`)
	h.Set("Content-Type", "image/png")
	pw, _ := w.CreatePart(h)
	_, _ = pw.Write([]byte("\x89PNG pixels"))
	_ = w.Close()

	model, withModel := formModel(body.Bytes(), w.Boundary())
	if model != "vllm/Qwen/Qwen-Image-Edit-2511" {
		t.Fatalf("model %q", model)
	}
	r := multipart.NewReader(bytes.NewReader(withModel("Qwen/Qwen-Image-Edit-2511")), w.Boundary())
	got := map[string]string{}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		got[p.FormName()] = string(data)
		if p.FormName() == "image" && (p.FileName() != "kite.png" || p.Header.Get("Content-Type") != "image/png") {
			t.Errorf("image part %v", p.Header)
		}
	}
	want := map[string]string{"prompt": "make it blue", "model": "Qwen/Qwen-Image-Edit-2511", "image": "\x89PNG pixels"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	var noModel bytes.Buffer
	w = multipart.NewWriter(&noModel)
	_ = w.WriteField("prompt", "make it blue")
	_ = w.Close()
	if model, _ := formModel(noModel.Bytes(), w.Boundary()); model != "" {
		t.Errorf("no model: %q", model)
	}
	if model, _ := formModel([]byte("not a form"), "x"); model != "" {
		t.Errorf("not a form: %q", model)
	}
}
