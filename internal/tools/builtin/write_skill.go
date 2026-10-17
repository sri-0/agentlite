package builtin

import (
	"fmt"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/internal/skills"
	"agentlite/pkg/toolx"
)

// writeSkillTool names the tool that writes a skill to the library.
const writeSkillTool = "write_skill"

type writeSkillArgs struct {
	Name         string         `json:"name" jsonschema:"lowercase letters, digits and single hyphens, at most 64 characters"`
	Description  string         `json:"description" jsonschema:"what the skill does and when to use it, at most 1024 characters"`
	Instructions string         `json:"instructions" jsonschema:"the instructions, in Markdown: the body of SKILL.md"`
	Files        []skillFileArg `json:"files,omitempty" jsonschema:"the skill's other files"`
	Title        string         `json:"title,omitempty" jsonschema:"a name to show people, e.g. Incident reports"`
	Category     string         `json:"category,omitempty" jsonschema:"what kind of skill it is, e.g. Writing; ask the user"`
	Source       string         `json:"source,omitempty" jsonschema:"where it comes from, e.g. Internal or Open source; ask the user"`
}

type skillFileArg struct {
	Path    string `json:"path" jsonschema:"the file's path in the skill, e.g. references/example.md"`
	Content string `json:"content" jsonschema:"the file's full text"`
}

// writeSkill creates a library skill, or replaces one's SKILL.md and the
// files given, after the user approves; its other files are kept, and so are
// its title, category and source unless given.
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
		for _, f := range a.Files {
			files[f.Path] = []byte(f.Content)
		}
		files[skills.SkillFile] = md // the instructions, not a file of that path
		s, err := d.Skills.Save(tc, skills.Draft{
			Files: files, Meta: skills.Meta{Title: given(a.Title), Category: given(a.Category), Source: given(a.Source)},
			KeepFiles: true,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": s.ID, "name": s.Name, "files": len(s.Files)}, nil
	})
}

// given is a detail the model gave; nil keeps what the skill has.
func given(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
