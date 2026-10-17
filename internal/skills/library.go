package skills

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/pkg/db/opensearch"
)

// Store keeps the library; opensearch.Client is one.
type Store interface {
	SaveSkill(ctx context.Context, s *opensearch.Skill) error
	Skill(ctx context.Context, name string) (*opensearch.Skill, error)
	Skills(ctx context.Context, f opensearch.SkillFilter) (*opensearch.SkillList, error)
	DeleteSkill(ctx context.Context, name string) error
}

// Library is the skills library over its store. Agents read it as an adk
// skill.Source, through adk's skill toolset and /name.
type Library struct {
	Store
}

var _ skill.Source = (*Library)(nil)

// File returns one of a skill's files.
func (l *Library) File(ctx context.Context, name, p string) ([]byte, error) {
	s, err := l.load(ctx, name)
	if err != nil {
		return nil, err
	}
	return file(s, p)
}

// file is the file of s at p; ./references/x.md is references/x.md.
func file(s *opensearch.Skill, p string) ([]byte, error) {
	p = path.Clean(p)
	i := slices.IndexFunc(s.Files, func(f opensearch.SkillFile) bool { return f.Path == p })
	if i < 0 {
		return nil, fmt.Errorf("%w: %s has no %s", skill.ErrResourceNotFound, s.Name, p)
	}
	return s.Files[i].Content, nil
}

// ListFrontmatters lists every skill's name and description.
func (l *Library) ListFrontmatters(ctx context.Context) ([]*skill.Frontmatter, error) {
	list, err := l.Skills(ctx, opensearch.SkillFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]*skill.Frontmatter, len(list.Skills))
	for i, s := range list.Skills {
		out[i] = &skill.Frontmatter{Name: s.Name, Description: s.Description}
	}
	return out, nil
}

// LoadFrontmatter returns a skill's name and description.
func (l *Library) LoadFrontmatter(ctx context.Context, name string) (*skill.Frontmatter, error) {
	s, err := l.load(ctx, name)
	if err != nil {
		return nil, err
	}
	return &skill.Frontmatter{Name: s.Name, Description: s.Description}, nil
}

// LoadInstructions returns the body of a skill's SKILL.md.
func (l *Library) LoadInstructions(ctx context.Context, name string) (string, error) {
	md, err := l.File(ctx, name, SkillFile)
	if err != nil {
		return "", err
	}
	_, body, err := parse(md)
	return body, err
}

// ListResources lists a skill's files other than SKILL.md under dir.
func (l *Library) ListResources(ctx context.Context, name, dir string) ([]string, error) {
	s, err := l.load(ctx, name)
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

// LoadResource opens one of a skill's text files: the model reads what it
// returns, so a binary file is refused.
func (l *Library) LoadResource(ctx context.Context, name, p string) (io.ReadCloser, error) {
	b, err := l.File(ctx, name, p)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%s is a binary file: it cannot be read", p)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (l *Library) load(ctx context.Context, name string) (*opensearch.Skill, error) {
	s, err := l.Skill(ctx, name)
	if errors.Is(err, opensearch.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", skill.ErrSkillNotFound, name)
	}
	return s, err
}

// Save stores a skill, replacing any of the same name. A replaced skill keeps
// when it was created, and its title, category and source unless s has its
// own.
func (l *Library) Save(ctx context.Context, s *opensearch.Skill) error {
	now := time.Now()
	s.CreatedAt, s.UpdatedAt = now, now
	prev, err := l.Skill(ctx, s.Name)
	switch {
	case err == nil:
		if !prev.CreatedAt.IsZero() {
			s.CreatedAt = prev.CreatedAt
		}
		s.Title, s.Category, s.Source = cmp.Or(s.Title, prev.Title), cmp.Or(s.Category, prev.Category),
			cmp.Or(s.Source, prev.Source)
	case !errors.Is(err, opensearch.ErrNotFound):
		return err
	}
	return l.SaveSkill(ctx, s)
}

// Update sets m on a stored skill. It writes the skill it read back whole,
// so an upload of the same skill at the same moment can be lost; the library
// is edited by people, one at a time.
func (l *Library) Update(ctx context.Context, name string, m Meta) (*opensearch.Skill, error) {
	s, err := l.Skill(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := m.Apply(s); err != nil {
		return nil, err
	}
	s.UpdatedAt = time.Now()
	return s, l.SaveSkill(ctx, s)
}
