package suggest

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"agentlite/pkg/db/opensearch"
)

func TestLastTurn(t *testing.T) {
	user := opensearch.Message{Role: "user", Text: "q"}
	answer := opensearch.Message{Role: "assistant", Text: "a"}
	for _, tc := range []struct {
		name string
		msgs []opensearch.Message
		ok   bool
	}{
		{"answered", []opensearch.Message{user, answer}, true},
		{"no messages", nil, false},
		{"only the message", []opensearch.Message{user}, false},
		{"empty answer (running or paused)", []opensearch.Message{user, {Role: "assistant", Text: "  "}}, false},
		{"answer without its message", []opensearch.Message{answer, answer}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, ok := lastTurn(tc.msgs)
			if ok != tc.ok || (ok && (m != "q" || a != "a")) {
				t.Errorf("lastTurn = %q, %q, %v; want ok %v", m, a, ok, tc.ok)
			}
		})
	}
}

// TestPrompt: both texts, each cut to textCap runes.
func TestPrompt(t *testing.T) {
	long := strings.Repeat("é", textCap+50)
	p := prompt("short question", long)
	if !strings.Contains(p, "short question") || strings.Contains(p, long) {
		t.Fatalf("prompt didn't keep the question or cut the answer")
	}
	if got := utf8.RuneCountInString(clip(long, textCap)); got != textCap {
		t.Errorf("clipped to %d runes, want %d", got, textCap)
	}
}

func TestParse(t *testing.T) {
	got, err := parse(`{"suggestions": ["  one ", "", "two", "three", "four"]}`)
	if err != nil || strings.Join(got, "|") != "one|two|three" {
		t.Errorf("parse = %q, %v; want the first three non-empty, trimmed", got, err)
	}
	got, _ = parse(`{"suggestions": ["` + strings.Repeat("x", 200) + `"]}`)
	if utf8.RuneCountInString(got[0]) != maxLen || !strings.HasSuffix(got[0], "…") {
		t.Errorf("a long suggestion is %d runes, want %d ending in …", utf8.RuneCountInString(got[0]), maxLen)
	}
	if _, err := parse("Sure! Here are some ideas"); !errors.Is(err, errModel) {
		t.Errorf("prose: err %v, want errModel", err)
	}
}
