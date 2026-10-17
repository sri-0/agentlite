package skills

import (
	"errors"
	"strings"
	"testing"

	"agentlite/pkg/db/opensearch"
)

// TestSetDescription: a new description changes that value alone, so what
// else an author wrote in the frontmatter (Claude Code fields, nested
// metadata in their order, comments, literals) survives, and so does the body.
func TestSetDescription(t *testing.T) {
	md := "---\nname: demo\n# shown in the picker\ndescription: Old one.\nargument-hint: \"[file]\"\n" +
		"metadata:\n  zeta: \"1\"\n  alpha: \"2\"\nmode: 0755\n---\n# Body\n\nUnchanged.\n"
	for _, tc := range []struct {
		name, desc string
		want       []string // in this order in the new SKILL.md
		parsed     string   // the description read back from it
		unchanged  bool
		err        error
	}{
		{
			name: "one line", desc: "New **one**.", parsed: "New **one**.",
			want: []string{
				"name: demo", "# shown in the picker", "description: New **one**.", "argument-hint: \"[file]\"",
				"zeta:", "alpha:", "mode: 0755\n---\n# Body\n\nUnchanged.\n",
			},
		},
		{
			name: "several lines", desc: "First.\r\n\r\nUse when testing.\n", parsed: "First.\n\nUse when testing.",
			want: []string{"description: |-\n  First.", "zeta:", "alpha:"},
		},
		{name: "the same, as a form sends it", desc: "Old one.\r\n", unchanged: true},
		{name: "too long", desc: strings.Repeat("x", 1100), err: ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []opensearch.SkillFile{{Path: SkillFile, Content: []byte(md)}}
			s := &opensearch.Skill{Name: "demo", Description: "Old one.", Files: files}
			err := Meta{Description: &tc.desc}.Apply(s)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			got := string(s.Files[0].Content)
			if tc.parsed != "" && s.Description != tc.parsed {
				t.Errorf("description %q, want %q", s.Description, tc.parsed)
			}
			if tc.unchanged && got != md {
				t.Errorf("rewritten:\n%s", got)
			}
			at := 0
			for _, w := range tc.want {
				i := strings.Index(got[at:], w)
				if i < 0 {
					t.Fatalf("%q missing, or out of order, in:\n%s", w, got)
				}
				at += i + len(w)
			}
		})
	}
}
