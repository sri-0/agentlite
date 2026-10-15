package builtin

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/internal/skills"
	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/toolx"
)

// writeSkillTool names the tool that writes a skill; adk's skill toolset reads them.
const writeSkillTool = "write_skill"

type writeSkillArgs struct {
	Name         string         `json:"name" jsonschema:"lowercase letters, digits and single hyphens, at most 64 characters"`
	Description  string         `json:"description" jsonschema:"what the skill does and when to use it, at most 1024 characters"`
	Instructions string         `json:"instructions" jsonschema:"the instructions, in Markdown: the body of SKILL.md"`
	Files        []skillFileArg `json:"files,omitempty" jsonschema:"the skill's other files"`
}

type skillFileArg struct {
	Path    string `json:"path" jsonschema:"the file's path in the skill, e.g. references/example.md"`
	Content string `json:"content" jsonschema:"the file's full text"`
}

// writeSkill creates a library skill, or replaces one's SKILL.md and the
// files given, after the user approves; its other files are kept.
func writeSkill(d Deps) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: writeSkillTool,
		Description: "Create a skill in the skills library, or change one, after the user approves. " +
			"Write the whole skill in one call: its instructions, and any other files it has in files. " +
			"An existing skill keeps the other files you do not give.",
		RequireConfirmation: true,
	}, func(tc toolx.Context, a writeSkillArgs) (map[string]any, error) {
		md, err := skill.Build(&skill.Frontmatter{Name: a.Name, Description: a.Description}, a.Instructions)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", skills.ErrInvalid, err)
		}
		files := map[string][]byte{}
		prev, err := d.Skills.Skill(tc, a.Name)
		switch {
		case err == nil:
			for _, f := range prev.Files {
				files[f.Path] = f.Content
			}
		case !errors.Is(err, opensearch.ErrNotFound):
			return nil, err
		}
		for _, f := range a.Files {
			files[f.Path] = []byte(f.Content)
		}
		files[skills.SkillFile] = md
		s, err := skills.New(files)
		if err != nil {
			return nil, err
		}
		if err := d.Skills.SaveSkill(tc, s); err != nil {
			return nil, err
		}
		return map[string]any{"name": s.Name, "files": slices.Sorted(maps.Keys(files))}, nil
	})
}
