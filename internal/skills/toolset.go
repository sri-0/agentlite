package skills

import (
	"context"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
)

// ToolsetName is the toolset's name, by which an agent's tools list gives it.
const ToolsetName = "skills"

// Toolset is adk's skill toolset over the library: list_skills, load_skill
// (a skill's instructions) and load_skill_resource (one of its files), and
// before each model call the skills' names and descriptions at the end of the
// system instruction. Go's adk has no tool that runs a skill's scripts.
func (l *Library) Toolset(ctx context.Context) (tool.Toolset, error) {
	return skilltoolset.New(ctx, skilltoolset.Config{Name: ToolsetName, Source: l, SystemInstruction: instruction})
}

// instruction replaces adk's own, which says scripts can be run. Its third
// point is the <skill_content> block Resolve gives a /name.
const instruction = `You can use skills: instructions (a SKILL.md) and other files for specialised tasks, kept in a library. available_skills lists each skill's name and what it is for, not its instructions.
1. If a skill fits the task, you MUST call load_skill with its name before doing the task, then follow its instructions exactly.
2. Read the files its instructions point to with load_skill_resource. A skill's scripts are for reading only: nothing can run them.
3. A user message that names a skill as /<name> carries its instructions in a <skill_content> block: follow them without loading the skill again. In an earlier message the block may be gone: load the skill if you still need it.`
