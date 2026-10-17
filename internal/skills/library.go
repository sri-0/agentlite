// Package skills is the skills library: Agent Skills, each a SKILL.md and any
// other files (https://agentskills.io/specification). A skill's record is in
// OpenSearch, a uuid its id and its name unique; its files are in S3 at
// skills/<id>/<path>. Agents read the library through adk's skill toolset and
// /name, and npx skills installs from it. Nothing runs a skill's scripts.
package skills

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"path"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// Library is the skills library.
type Library struct {
	records *opensearch.Client
	files   *s3.Client
	toolset tool.Toolset
	log     zerolog.Logger
}

// New returns the library over its records and files.
func New(ctx context.Context, records *opensearch.Client, files *s3.Client, log zerolog.Logger) (*Library, error) {
	l := &Library{records: records, files: files, log: log}
	var err error
	l.toolset, err = skilltoolset.New(ctx, skilltoolset.Config{Name: "skills", Source: l, SystemInstruction: instruction})
	return l, err
}

// Draft is a skill to save: its files and the details it is filed with.
type Draft struct {
	Files map[string][]byte // by path, SKILL.md among them
	Meta
	KeepFiles bool // keep a replaced skill's files that Files leaves out
}

// Save stores a skill. One of the same name is replaced: it keeps its id,
// when it was created, the details d leaves out and, with KeepFiles, the
// files d leaves out. Two first saves of one name at the same moment could
// make two skills; the library is written by people, and by agents a person
// approves, one at a time.
func (l *Library) Save(ctx context.Context, d Draft) (*opensearch.Skill, error) {
	files := maps.Clone(d.Files)
	if d.Description != nil {
		md, err := describe(files[SkillFile], *d.Description)
		if err != nil {
			return nil, err
		}
		files[SkillFile] = md
	}
	fm, err := check(files)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &opensearch.Skill{ID: uuid.NewString(), Name: fm.Name, Description: fm.Description, CreatedAt: now, UpdatedAt: now}
	prev, err := l.records.SkillNamed(ctx, s.Name)
	switch {
	case err == nil:
		s.ID, s.CreatedAt = prev.ID, prev.CreatedAt
		s.Title, s.Category, s.Source = prev.Title, prev.Category, prev.Source
	case !errors.Is(err, opensearch.ErrNotFound):
		return nil, err
	}
	d.details(s)
	for p, b := range files {
		ctype := cmp.Or(mime.TypeByExtension(path.Ext(p)), "application/octet-stream")
		if err := l.files.Put(ctx, key(s.ID, p), ctype, b); err != nil {
			return nil, err
		}
		s.Files = append(s.Files, opensearch.SkillFile{Path: p, Size: len(b)})
	}
	var stale []string
	if prev != nil {
		for _, f := range prev.Files {
			if _, given := files[f.Path]; given {
				continue
			}
			if d.KeepFiles {
				s.Files = append(s.Files, f)
			} else {
				stale = append(stale, key(s.ID, f.Path))
			}
		}
	}
	slices.SortFunc(s.Files, func(a, b opensearch.SkillFile) int { return cmp.Compare(a.Path, b.Path) })
	if err := l.records.SaveSkill(ctx, s); err != nil {
		return nil, err
	}
	// The skill is saved and no longer lists them: a file left behind goes
	// with the rest when the skill is deleted.
	if err := l.files.Delete(ctx, stale...); err != nil {
		l.log.Warn().Err(err).Str("skill", s.Name).Msg("delete replaced files")
	}
	return s, nil
}

// Update sets m on a skill.
func (l *Library) Update(ctx context.Context, id string, m Meta) (*opensearch.Skill, error) {
	s, err := l.records.Skill(ctx, id)
	if err != nil {
		return nil, err
	}
	md, err := l.File(ctx, s, SkillFile)
	if err != nil {
		return nil, err
	}
	return l.Save(ctx, Draft{Files: map[string][]byte{SkillFile: md}, Meta: m, KeepFiles: true})
}

// Get fetches a skill by id.
func (l *Library) Get(ctx context.Context, id string) (*opensearch.Skill, error) {
	return l.records.Skill(ctx, id)
}

// Named fetches a skill by name; adk's skill.ErrSkillNotFound when there is
// none.
func (l *Library) Named(ctx context.Context, name string) (*opensearch.Skill, error) {
	s, err := l.records.SkillNamed(ctx, name)
	if errors.Is(err, opensearch.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", skill.ErrSkillNotFound, name)
	}
	return s, err
}

// List lists the library as f narrows it, with its categories and sources.
func (l *Library) List(ctx context.Context, f opensearch.SkillFilter) (*opensearch.SkillList, error) {
	return l.records.Skills(ctx, f)
}

// Delete removes a skill and its files.
func (l *Library) Delete(ctx context.Context, id string) error {
	if err := l.records.DeleteSkill(ctx, id); err != nil {
		return err
	}
	return l.files.DeletePrefix(ctx, key(id, ""))
}

// File reads one of a skill's files; ./references/x.md is references/x.md.
func (l *Library) File(ctx context.Context, s *opensearch.Skill, p string) ([]byte, error) {
	p = path.Clean(p)
	if !slices.ContainsFunc(s.Files, func(f opensearch.SkillFile) bool { return f.Path == p }) {
		return nil, fmt.Errorf("%w: %s has no %s", skill.ErrResourceNotFound, s.Name, p)
	}
	r, err := l.files.Get(ctx, key(s.ID, p))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// key is where a skill's file is in S3.
func key(id, p string) string {
	return "skills/" + id + "/" + p
}
