package skills

import (
	"context"
	"errors"
	"fmt"

	"agentlite/internal/refs"
	"agentlite/internal/stream"
)

// activity is the skill activity's content: the skill a /name loaded.
type activity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
}

var _ refs.Resolver = (*Library)(nil)

// Resolve resolves /name: the skill's instructions, in a <skill_content>
// block, and a skill activity.
func (l *Library) Resolve(ctx context.Context, name string) (refs.Ref, bool, error) {
	s, err := l.Named(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return refs.Ref{}, false, nil
	}
	if err != nil {
		return refs.Ref{}, false, err
	}
	body, err := l.instructions(ctx, s)
	if err != nil {
		return refs.Ref{}, false, err
	}
	return refs.Ref{
		Kind: stream.ActivitySkill, ID: s.ID,
		Content:  fmt.Sprintf("<skill_content name=%q>\n%s\n</skill_content>", s.Name, body),
		Activity: activity{ID: s.ID, Name: s.Name, Title: s.Title, Description: s.Description},
	}, true, nil
}
