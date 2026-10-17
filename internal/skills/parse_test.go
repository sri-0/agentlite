package skills

import (
	"archive/zip"
	"bytes"
	"errors"
	"maps"
	"slices"
	"testing"
)

// TestRead: what an upload is read as, and what is refused.
func TestRead(t *testing.T) {
	md := "---\nname: demo\ndescription: A demo.\n---\nHi.\n"
	zipOf := func(files map[string]string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, name := range slices.Sorted(maps.Keys(files)) {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(files[name]))
		}
		zw.Close()
		return buf.Bytes()
	}
	for _, tc := range []struct {
		name  string
		data  []byte
		paths []string
		err   error
	}{
		{name: "SKILL.md alone", data: []byte(md), paths: []string{"SKILL.md"}},
		{
			name:  "zip of the skill's folder, macOS litter left out",
			data:  zipOf(map[string]string{"demo/SKILL.md": md, "demo/refs/a.md": "a", "demo/.DS_Store": "x", "__MACOSX/demo/._SKILL.md": "x"}),
			paths: []string{"SKILL.md", "refs/a.md"},
		},
		{name: "zip with SKILL.md at its root", data: zipOf(map[string]string{"SKILL.md": md}), paths: []string{"SKILL.md"}},
		{name: "two top folders", data: zipOf(map[string]string{"a/SKILL.md": md, "b/SKILL.md": md}), err: ErrInvalid},
		{name: "a path out of the zip", data: zipOf(map[string]string{"SKILL.md": md, "../evil.md": "x"}), err: ErrInvalid},
		{name: "not a zip", data: []byte("PK\x03\x04broken"), err: ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := Read(tc.data)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if got := slices.Sorted(maps.Keys(files)); !slices.Equal(got, tc.paths) {
				t.Errorf("paths %v, want %v", got, tc.paths)
			}
		})
	}
}
