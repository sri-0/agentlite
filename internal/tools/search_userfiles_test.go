package tools

import (
	"encoding/json"
	"testing"

	"agentlite/internal/files"
)

// TestFoundIn: the hits of a search_userfiles result, as the tool returned
// them and as they read back from the session (JSON); none from an error.
func TestFoundIn(t *testing.T) {
	hits := []files.Hit{
		{FileID: "f1", Name: "a.png", MediaType: "image/png"},
		{FileID: "f2", Name: "b.md", MediaType: "text/markdown", Text: "x"},
	}
	var decoded map[string]any
	raw, _ := json.Marshal(map[string]any{"hits": hits})
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]map[string]any{"returned": {"hits": hits}, "read back": decoded} {
		if got := foundIn(response); len(got) != 2 || got[0] != hits[0] || got[1] != hits[1] {
			t.Errorf("%s: got %+v", name, got)
		}
	}
	if got := foundIn(map[string]any{"error": "boom"}); len(got) != 0 {
		t.Errorf("an error: got %+v", got)
	}
}
