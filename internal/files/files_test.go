package files

import (
	"errors"
	"strings"
	"testing"

	"agentlite/pkg/db/opensearch"
)

// TestSniff: the first bytes decide, the extension only refines plain text
// and an MP3 without an ID3 tag; any other kind is refused.
func TestSniff(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string // want "" is refused
	}{
		{"notes.txt", "hello", "text/plain; charset=utf-8"},
		{"notes.md", "# Title", "text/markdown; charset=utf-8"},
		{"data.csv", "a,b\n1,2", "text/csv; charset=utf-8"},
		{"data.json", `{"a":1}`, "application/json"},
		{"main.go", "package main", "text/plain; charset=utf-8"},
		{"page.html", "<html><body>hi</body></html>", "text/html; charset=utf-8"},
		{"photo.png", "\x89PNG\r\n\x1a\n0000", "image/png"},
		{"photo.jpg", "\xff\xd8\xff\xe0", "image/jpeg"},
		{"anim.gif", "GIF89a", "image/gif"},
		{"tone.wav", "RIFF\x24\x00\x00\x00WAVEfmt ", "audio/wave"},
		{"tone.mp3", "ID3\x04\x00", "audio/mpeg"},
		{"raw.mp3", "\xff\xfb\x90\x00\x01\x02", "audio/mpeg"},
		{"clip.mp4", "\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41", "video/mp4"},
		{"doc.pdf", "%PDF-1.4", "application/pdf"},
		{"notes.md.zip", "PK\x03\x04", ""},
		{"clip.webm", "\x1a\x45\xdf\xa3", ""},
		{"blob.bin", "\x00\x01\x02\x03", ""},
		{"raw.mp4", "\xff\xfb\x90\x00", ""}, // only .mp3 is taken on its extension
	} {
		got, err := sniff(tc.name, []byte(tc.body))
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

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf": "report.pdf", "/tmp/x/report.pdf": "report.pdf", `C:\Users\me\report.pdf`: "report.pdf",
		"  spaced.txt ": "spaced.txt", "": "file", "/": "file", strings.Repeat("é", 300): strings.Repeat("é", maxName),
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReference: images and PDFs go as they are, short text inline, and long
// text, audio and video as a note, which offers search only on a ready file.
func TestReference(t *testing.T) {
	file := func(mediaType string, size int, status string) *opensearch.File {
		return &opensearch.File{ID: "f1", Name: "x", MediaType: mediaType, Size: size, Status: status}
	}
	long := strings.Repeat("a", inlineText+1)
	for _, tc := range []struct {
		name    string
		f       *opensearch.File
		body    string
		part    string // the part's media type, "" for none
		content string // what Content starts with
		search  bool   // the note offers search_userfiles
	}{
		{"image", file("image/png", 4, opensearch.FileReady), "\x89PNG", "image/png", "#f1 is the file", false},
		{"pdf", file("application/pdf", 4, opensearch.FileStored), "%PDF", "application/pdf", "#f1 is the file", false},
		{"short text", file("text/markdown; charset=utf-8", 2, opensearch.FileReady), "hi", "", "<file id=\"f1\" name=\"x\">\nhi\n</file>", false},
		{"text at the limit", file("text/plain; charset=utf-8", inlineText, opensearch.FileReady), long[:inlineText], "", "<file", false},
		{"long text", file("text/plain; charset=utf-8", len(long), opensearch.FileReady), long, "", "#f1 is the file", true},
		{"text not read", file("text/plain; charset=utf-8", 1<<20, opensearch.FileReady), "", "", "#f1 is the file", true},
		{"audio", file("audio/wave", 10, opensearch.FileReady), "", "", "#f1 is the file", true},
		{"video, not embedded", file("video/mp4", 10, opensearch.FileStored), "", "", "#f1 is the file", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.body != "" {
				body = []byte(tc.body)
			}
			ref := reference(tc.f, body)
			switch {
			case !strings.HasPrefix(ref.Content, tc.content):
				t.Errorf("content %.80q, want it to start %q", ref.Content, tc.content)
			case tc.part == "" && ref.Part != nil, tc.part != "" && (ref.Part == nil || ref.Part.InlineData.MIMEType != tc.part):
				t.Errorf("part %+v, want %q", ref.Part, tc.part)
			case strings.Contains(ref.Content, "search_userfiles") != tc.search:
				t.Errorf("content %q: search offered = %v, want %v", ref.Content, !tc.search, tc.search)
			case ref.Kind != "file" || ref.ID != "f1":
				t.Errorf("kind %q id %q", ref.Kind, ref.ID)
			}
		})
	}
}
