package agents

import (
	"context"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
)

// skillsToolset is adk's skill toolset over the library: list_skills,
// load_skill and load_skill_resource, with the skills listed in the system
// instruction before each model call. Go's adk has no tool that runs a
// skill's scripts.
func (rt *Runtime) skillsToolset() (tool.Toolset, error) {
	return skilltoolset.New(context.Background(), skilltoolset.Config{
		Name: "skills", Source: rt.skills, SystemInstruction: skillsInstruction,
	})
}

// skillActivity is the skill activity's content: the skill a /name message
// loaded.
type skillActivity struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// skillsInstruction replaces adk's own, which says scripts can be run.
const skillsInstruction = `You can use skills: instructions (a SKILL.md) and other files for specialised tasks, kept in a library. available_skills lists each skill's name and what it is for, not its instructions.
1. If a skill fits the task, you MUST call load_skill with its name before doing the task, then follow its instructions exactly.
2. Read the files its instructions point to with load_skill_resource. A skill's scripts are for reading only: nothing can run them.
3. A user message that starts with /<name> already carries that skill's instructions in a <skill_content> block: follow them without loading the skill again.
4. To create or change a skill, use write_skill.`
