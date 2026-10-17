// Package skilltools holds the skills library's tools: adk's skill toolset
// (list_skills, load_skill, load_skill_resource) and write_skill.
package skilltools

import (
	"context"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"

	"agentlite/internal/skills"
)

// Name is the toolset's name, by which an agent's tools list gives it.
const Name = "skills"

// Toolset is adk's skill toolset over the library: list_skills, load_skill
// (a skill's instructions) and load_skill_resource (one of its files), and
// before each model call the skills' names and descriptions at the end of the
// system instruction. Go's adk has no tool that runs a skill's scripts.
func Toolset(ctx context.Context, lib *skills.Library) (tool.Toolset, error) {
	return skilltoolset.New(ctx, skilltoolset.Config{Name: Name, Source: lib, SystemInstruction: instruction})
}

// instruction replaces adk's own, which says scripts can be run.
const instruction = `You can use skills: instructions (a SKILL.md) and other files for specialised tasks, kept in a library. available_skills lists each skill's name and what it is for, not its instructions.
1. If a skill fits the task, you MUST call load_skill with its name before doing the task, then follow its instructions exactly.
2. Read the files its instructions point to with load_skill_resource. A skill's scripts are for reading only: nothing can run them.
3. A user message that names a skill as /<name> carries its instructions in a <skill_content> block: follow them without loading the skill again. In an earlier message the block may be gone: load the skill if you still need it.`
