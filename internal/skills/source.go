package skills

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/pkg/db/opensearch"
)

// The library is an adk skill.Source, which adk's skill toolset reads. Each
// call has the run's context; OpenSearch is read as the service today, and
// once it is read as the user, the listing holds only the skills they may see.
var _ skill.Source = (*Library)(nil)

// ListFrontmatters lists every skill's name and description.
func (l *Library) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	all, err := l.records.AllSkills(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, len(all))
	for i, s := range all {
		out[i] = &skill.Frontmatter{Name: s.Name, Description: s.Description}
	}
	return out, nil
}

// LoadFrontmatter returns a skill's name and description.
func (l *Library) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	s, err := l.Named(ctx, name)
	if err != nil {
		return nil, err
	}
	return &skill.Frontmatter{Name: s.Name, Description: s.Description}, nil
}

// LoadInstructions returns the body of a skill's SKILL.md.
func (l *Library) LoadInstructions(ctx context.Context, name string) (string, error) {
	s, err := l.Named(ctx, name)
	if err != nil {
		return "", err
	}
	return l.instructions(ctx, s)
}

func (l *Library) instructions(ctx context.Context, s *opensearch.Skill) (string, error) {
	md, err := l.File(ctx, s, SkillFile)
	if err != nil {
		return "", err
	}
	_, body, err := parse(md)
	return body, err
}

// ListResources lists a skill's files other than SKILL.md under dir.
func (l *Library) ListResources(ctx context.Context, name, dir string) ([]string, error) {
	s, err := l.Named(ctx, name)
	if err != nil {
		return nil, err
	}
	dir = path.Clean(dir)
	var out []string
	for _, f := range s.Files {
		inDir := dir == "." || strings.HasPrefix(f.Path, dir+"/")
		if f.Path != SkillFile && inDir {
			out = append(out, f.Path)
		}
	}
	return out, nil
}

// LoadResource returns one of a skill's text files, a script's as any other:
// the model reads it, and nothing can run it. A binary file is refused.
func (l *Library) LoadResource(ctx context.Context, name, p string) (io.ReadCloser, error) {
	s, err := l.Named(ctx, name)
	if err != nil {
		return nil, err
	}
	b, err := l.File(ctx, s, p)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%s is a binary file: it cannot be read", p)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
