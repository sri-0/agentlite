package skills

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"agentlite/pkg/db/opensearch"
)

// invocation is a message that starts with /name.
var invocation = regexp.MustCompile(`^/([a-z0-9-]+)(?:\s+|$)`)

// Expand is a turn's user content. A message that starts with /name, where
// name is a skill of the library, carries the skill's instructions in a
// <skill_content> part ahead of the rest of the message, and the skill is
// returned. Any other message is its text as typed.
func (l *Library) Expand(ctx context.Context, text string) (*genai.Content, *opensearch.Skill, error) {
	plain := genai.NewContentFromText(text, genai.RoleUser)
	m := invocation.FindStringSubmatch(text)
	if m == nil {
		return plain, nil, nil
	}
	s, err := l.load(ctx, m[1])
	switch {
	case errors.Is(err, skill.ErrSkillNotFound):
		return plain, nil, nil
	case err != nil:
		return nil, nil, err
	}
	md, err := file(s, SkillFile)
	if err != nil {
		return nil, nil, err
	}
	_, body, err := parse(md)
	if err != nil {
		return nil, nil, err
	}
	loaded := fmt.Sprintf("<skill_content name=%q>\n%s\n</skill_content>", s.Name, body)
	parts := []*genai.Part{genai.NewPartFromText(loaded)}
	if rest := strings.TrimSpace(text[len(m[0]):]); rest != "" {
		parts = append(parts, genai.NewPartFromText(rest))
	}
	return genai.NewContentFromParts(parts, genai.RoleUser), s, nil
}
