package userfiles

import (
	"errors"
	"strings"
	"testing"

	"agentlite/config"
)

// TestSniff: the content decides; any kind but text, images, audio, video
// and PDF is refused, and so is text that is not UTF-8.
func TestSniff(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string // want "" is refused
	}{
		{"text", "hello", "text/plain; charset=utf-8"},
		{"csv", "a,b\n1,2\n", "text/csv"},
		{"json", `{"a":1}`, "application/json"},
		{"html", "<html><body>hi</body></html>", "text/html; charset=utf-8"},
		{"png", "\x89PNG\r\n\x1a\n0000", "image/png"},
		{"jpeg", "\xff\xd8\xff\xe0", "image/jpeg"},
		{"webp", "RIFF\x24\x00\x00\x00WEBPVP8 ", "image/webp"},
		{"wav", "RIFF\x24\x00\x00\x00WAVEfmt ", "audio/wav"},
		{"mp3 without a tag", "\xff\xfb\x90\x00\x01\x02", "audio/mpeg"},
		{"mp4", "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41", "video/mp4"},
		{"pdf", "%PDF-1.4", "application/pdf"},
		{"zip", "PK\x03\x04", ""},
		{"binary", "\x00\x01\x02\x03", ""},
		{"utf-16", "\xff\xfeh\x00i\x00", ""},
		{"latin-1", "caf\xe9 au lait", ""},
	} {
		got, err := sniff([]byte(tc.body))
		if tc.want == "" {
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("%s: got %q, %v; want ErrUnsupported", tc.name, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestCleanTags: trimmed, blanks and repeats dropped; too many or too long
// is invalid.
func TestCleanTags(t *testing.T) {
	s := &Store{limits: config.FilesConfig{MaxTags: 3, MaxTagChars: 7}}
	got, err := s.cleanTags([]string{" q3 ", "", "finance", "q3", "  "})
	if err != nil || strings.Join(got, "|") != "finance|q3" {
		t.Errorf("cleanTags = %q, %v; want [finance q3]", got, err)
	}
	for name, tags := range map[string][]string{
		"too many": {"a", "b", "c", "d"},
		"too long": {"abcdefgh"},
	} {
		if _, err := s.cleanTags(tags); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.cleanTags([]string{"a", "b", "abcdefg"}); err != nil {
		t.Errorf("at the limits: %v", err)
	}
}
