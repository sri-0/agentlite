package suggest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPrompt: the last turn, the typed text, or both, each cut to textCap
// runes, blank ones and older messages left out.
func TestPrompt(t *testing.T) {
	long := strings.Repeat("é", textCap+50)
	turn := []Message{{"user", "older"}, {"user", "short question"}, {"assistant", long}}
	p := prompt(turn, "")
	if !strings.Contains(p, "The user wrote:\nshort question") || strings.Contains(p, "older") ||
		strings.Contains(p, long) || strings.Contains(p, "typing") {
		t.Fatalf("turn only: prompt didn't keep the last two, cut the answer, or leave typing out: %q", p[:80])
	}
	if p := prompt(nil, " how do I "); p != "The user is typing:\nhow do I" {
		t.Errorf("text only: prompt = %q", p)
	}
	if p := prompt([]Message{{"user", "q"}, {"assistant", "a"}}, "and then"); !strings.Contains(p, "answered:\na\n\nThe user is typing:\nand then") {
		t.Errorf("both: prompt = %q", p)
	}
	if got := utf8.RuneCountInString(clip(long, textCap)); got != textCap {
		t.Errorf("clipped to %d runes, want %d", got, textCap)
	}
}

// TestSuggestRefuses: the requests refused before any model is asked.
func TestSuggestRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Request
	}{
		{"nothing", Request{}},
		{"blank text", Request{Text: "  "}},
		{"blank messages", Request{Messages: []Message{{"assistant", " "}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(nil, "m", "").Suggest(context.Background(), tc.r); !errors.Is(err, ErrEmpty) {
				t.Errorf("err %v, want ErrEmpty", err)
			}
		})
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
