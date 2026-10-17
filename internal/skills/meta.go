package skills

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/adrg/frontmatter"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"

	"agentlite/pkg/db/opensearch"
)

// Meta is what a user sets on a skill besides its files. A nil field keeps
// what the skill has; "" clears a title, category or source. The description
// is SKILL.md's: setting it rewrites SKILL.md's frontmatter.
type Meta struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Category    *string `json:"category"`
	Source      *string `json:"source"`
}

// Apply sets m on s.
func (m Meta) Apply(s *opensearch.Skill) error {
	if m.Description != nil {
		desc := strings.TrimSpace(strings.ReplaceAll(*m.Description, "\r\n", "\n")) // as a browser's form sends it
		if desc != s.Description {
			if err := setDescription(s, desc); err != nil {
				return err
			}
		}
	}
	set := func(field, value *string) {
		if value != nil {
			*field = strings.TrimSpace(*value)
		}
	}
	set(&s.Title, m.Title)
	set(&s.Category, m.Category)
	set(&s.Source, m.Source)
	return nil
}

// rawFrontmatter hands over SKILL.md's frontmatter as it is written.
var rawFrontmatter = frontmatter.NewFormat("---", "---", func(b []byte, v any) error {
	*v.(*[]byte) = b
	return nil
})

// setDescription rewrites the description in s's SKILL.md. Only that value
// changes in the frontmatter's syntax tree, so the other fields keep their
// order, comments and formatting, and the body stays as it was.
func setDescription(s *opensearch.Skill, desc string) error {
	i := slices.IndexFunc(s.Files, func(f opensearch.SkillFile) bool { return f.Path == SkillFile })
	if i < 0 {
		return fmt.Errorf("%w: no %s", ErrInvalid, SkillFile)
	}
	var front []byte
	body, err := frontmatter.MustParse(bytes.NewReader(s.Files[i].Content), &front, rawFrontmatter)
	if err != nil {
		return fmt.Errorf("%w: %s frontmatter: %v", ErrInvalid, SkillFile, err)
	}
	tree, err := parser.ParseBytes(front, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("%w: %s frontmatter: %v", ErrInvalid, SkillFile, err)
	}
	value, err := yaml.MarshalWithOptions(desc, yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return err
	}
	path, err := yaml.PathString("$.description")
	if err != nil {
		return err
	}
	if err := path.ReplaceWithReader(tree, bytes.NewReader(value)); err != nil {
		return fmt.Errorf("%w: %s description: %v", ErrInvalid, SkillFile, err)
	}
	md := slices.Concat([]byte("---\n"), []byte(strings.TrimRight(tree.String(), "\n")), []byte("\n---\n"), body)
	fm, _, err := parse(md)
	if err != nil {
		return err
	}
	s.Files[i].Content, s.Description = md, fm.Description
	return nil
}
