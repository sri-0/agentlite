package suggest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSuggestRefuses: the requests refused before any model is asked.
func TestSuggestRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Request
	}{
		{"nothing", Request{}},
		{"blank text", Request{Text: "  "}},
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
