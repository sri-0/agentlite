package suggest

import (
	"context"
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

// TestPrompt: the turn, the typed text, or both, each cut to textCap runes.
func TestPrompt(t *testing.T) {
	long := strings.Repeat("é", textCap+50)
	p := prompt("short question", long, "")
	if !strings.Contains(p, "short question") || strings.Contains(p, long) || strings.Contains(p, "typing") {
		t.Fatalf("turn only: prompt didn't keep the question, cut the answer, or leave typing out: %q", p[:80])
	}
	if p := prompt("", "", "how do I"); p != "The user is typing:\nhow do I" {
		t.Errorf("text only: prompt = %q", p)
	}
	if p := prompt("q", "a", "and then"); !strings.Contains(p, "answered:\na\n\nThe user is typing:\nand then") {
		t.Errorf("both: prompt = %q", p)
	}
	if got := utf8.RuneCountInString(clip(long, textCap)); got != textCap {
		t.Errorf("clipped to %d runes, want %d", got, textCap)
	}
}

// history is a thread "t" whose messages are msgs.
type history []opensearch.Message

func (h history) Thread(_ context.Context, id string) (*opensearch.Thread, error) {
	if id != "t" {
		return nil, opensearch.ErrNotFound
	}
	return &opensearch.Thread{ID: id}, nil
}

func (h history) Messages(context.Context, string, int64, int) ([]opensearch.Message, error) {
	return h, nil
}

// TestSuggestRefuses: the requests refused before any model is asked.
func TestSuggestRefuses(t *testing.T) {
	unanswered := history{{Role: "user", Text: "q"}}
	for _, tc := range []struct {
		name string
		h    history
		r    Request
		want error
	}{
		{"nothing", nil, Request{}, ErrEmpty},
		{"blank text", nil, Request{Text: "  "}, ErrEmpty},
		{"unknown thread", nil, Request{ThreadID: "x"}, opensearch.ErrNotFound},
		{"unanswered thread, no text", unanswered, Request{ThreadID: "t"}, ErrNoAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(nil, tc.h, "m").Suggest(context.Background(), tc.r); !errors.Is(err, tc.want) {
				t.Errorf("err %v, want %v", err, tc.want)
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
