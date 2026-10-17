package skills

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/internal/refs"
	"agentlite/internal/stream"
)

// Toolset is adk's skill toolset over the library: list_skills, load_skill
// and load_skill_resource, and before each model call the skills' names and
// descriptions at the end of the system instruction. Go's adk has no tool
// that runs a skill's scripts.
func (l *Library) Toolset() tool.Toolset { return l.toolset }

// instruction replaces adk's own, which says scripts can be run.
const instruction = `You can use skills: instructions (a SKILL.md) and other files for specialised tasks, kept in a library. available_skills lists each skill's name and what it is for, not its instructions.
1. If a skill fits the task, you MUST call load_skill with its name before doing the task, then follow its instructions exactly.
2. Read the files its instructions point to with load_skill_resource. A skill's scripts are for reading only: nothing can run them.
3. A user message that names a skill as /<name> carries its instructions in a <skill_content> block: follow them without loading the skill again. In an earlier message the block may be gone: load the skill if you still need it.
4. To create or change a skill, use write_skill.`

// Activity is the skill activity's content: the skill a /name loaded.
type Activity struct {
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
	if errors.Is(err, skill.ErrSkillNotFound) {
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
		Activity: Activity{ID: s.ID, Name: s.Name, Title: s.Title, Description: s.Description},
	}, true, nil
}
