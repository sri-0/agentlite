package skills

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/adrg/frontmatter"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"

	"agentlite/pkg/db/opensearch"
)

// Meta is what a user sets on a skill besides its files. A nil field keeps
// what the skill has; "" clears a title, category or source. The description
// is SKILL.md's, as the spec has it: setting it rewrites SKILL.md.
type Meta struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Category    *string `json:"category"`
	Source      *string `json:"source"`
}

// apply sets m's title, category and source on s; the description is
// SKILL.md's (describe).
func (m Meta) apply(s *opensearch.Skill) {
	set := func(field, value *string) {
		if value != nil {
			*field = strings.TrimSpace(*value)
		}
	}
	set(&s.Title, m.Title)
	set(&s.Category, m.Category)
	set(&s.Source, m.Source)
}

// describe is SKILL.md with its description set to desc. A desc that only
// differs by surrounding space or CRLF line ends, as a browser's form sends
// it, leaves SKILL.md as it is.
func describe(md []byte, desc string) ([]byte, error) {
	fm, _, err := parse(md)
	if err != nil {
		return nil, err
	}
	desc = strings.TrimSpace(strings.ReplaceAll(desc, "\r\n", "\n"))
	if desc == fm.Description {
		return md, nil
	}
	out, err := withDescription(md, desc)
	if err != nil {
		return nil, fmt.Errorf("%w: %s description: %v", ErrInvalid, SkillFile, err)
	}
	return out, nil
}

// rawFrontmatter hands over SKILL.md's frontmatter as it is written.
var rawFrontmatter = frontmatter.NewFormat("---", "---", func(b []byte, v any) error {
	*v.(*[]byte) = b
	return nil
})

// withDescription replaces the description in SKILL.md's frontmatter. Only
// that value changes in the YAML's syntax tree, so the other fields keep
// their order, comments and formatting (0755 stays 0755), and the body stays
// as it was. A description of several lines is written as a literal block.
func withDescription(md []byte, desc string) ([]byte, error) {
	var front []byte
	body, err := frontmatter.MustParse(bytes.NewReader(md), &front, rawFrontmatter)
	if err != nil {
		return nil, err
	}
	tree, err := parser.ParseBytes(front, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	value, err := yaml.MarshalWithOptions(desc, yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return nil, err
	}
	path, err := yaml.PathString("$.description")
	if err != nil {
		return nil, err
	}
	if err := path.ReplaceWithReader(tree, bytes.NewReader(value)); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "---\n%s\n---\n%s", strings.TrimRight(tree.String(), "\n"), body), nil
}
