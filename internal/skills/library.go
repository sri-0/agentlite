// Package skills is the skills library: Agent Skills, each a SKILL.md and any
// other files (https://agentskills.io/specification). A skill's record is in
// OpenSearch, a uuid its id and its name unique; its files are in S3 at
// skills/<id>/<path>. Agents read the library through adk's skill toolset
// (Toolset) and /name, and npx skills installs from it. Nothing runs a
// skill's scripts.
package skills

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/pkg/db/opensearch"
	"agentlite/pkg/s3"
)

// Errors for a missing skill and a missing file: adk's own, which its skill
// toolset reports to the model.
var (
	ErrNotFound     = skill.ErrSkillNotFound
	ErrFileNotFound = skill.ErrResourceNotFound
)

// Library is the skills library.
type Library struct {
	records *opensearch.Client
	files   *s3.Client
	log     zerolog.Logger
}

// New returns the library over its records and files.
func New(records *opensearch.Client, files *s3.Client, log zerolog.Logger) *Library {
	return &Library{records: records, files: files, log: log}
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
	s := &opensearch.Skill{
		ID: uuid.Must(uuid.NewV7()).String(), Name: fm.Name, Description: fm.Description, CreatedAt: now, UpdatedAt: now,
	}
	prev, err := l.records.SkillNamed(ctx, s.Name)
	switch {
	case err == nil:
		s.ID, s.CreatedAt = prev.ID, prev.CreatedAt
		s.Title, s.Category, s.Source = prev.Title, prev.Category, prev.Source
	case !errors.Is(err, opensearch.ErrNotFound):
		return nil, err
	}
	d.apply(s)
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
	if err := l.store(ctx, s, files); err != nil {
		if prev == nil { // no record lists a new skill's files
			l.cleanUp(s.Name, l.files.DeletePrefix(context.WithoutCancel(ctx), key(s.ID, "")))
		}
		return nil, err
	}
	// The record no longer lists them; one left behind goes with the rest
	// when the skill is deleted.
	l.cleanUp(s.Name, l.files.Delete(context.WithoutCancel(ctx), stale...))
	return s, nil
}

// store writes a skill's files, then its record, which lists them.
func (l *Library) store(ctx context.Context, s *opensearch.Skill, files map[string][]byte) error {
	for p, b := range files {
		if err := l.files.Put(ctx, key(s.ID, p), ContentType(p, b), b); err != nil {
			return err
		}
		s.Files = append(s.Files, opensearch.SkillFile{Path: p, Size: len(b)})
	}
	slices.SortFunc(s.Files, func(a, b opensearch.SkillFile) int { return cmp.Compare(a.Path, b.Path) })
	return l.records.SaveSkill(ctx, s)
}

// cleanUp logs a failed delete of files no record lists: they waste space,
// and the save or delete that left them stands.
func (l *Library) cleanUp(skill string, err error) {
	if err != nil {
		l.log.Warn().Err(err).Str("skill", skill).Msg("delete skill files")
	}
}

// Update sets m on a skill.
func (l *Library) Update(ctx context.Context, id string, m Meta) (*opensearch.Skill, error) {
	return l.edit(ctx, id, map[string][]byte{}, m)
}

// WriteFile replaces one of a skill's files, or adds it.
func (l *Library) WriteFile(ctx context.Context, id, p string, b []byte) (*opensearch.Skill, error) {
	return l.edit(ctx, id, map[string][]byte{path.Clean(p): b}, Meta{})
}

// edit saves files and m on the skill with the id, which keeps its other
// files. A new SKILL.md must keep the skill's name: saved under another, it
// would be another skill.
func (l *Library) edit(ctx context.Context, id string, files map[string][]byte, m Meta) (*opensearch.Skill, error) {
	s, err := l.records.Skill(ctx, id)
	if err != nil {
		return nil, err
	}
	if md, ok := files[SkillFile]; ok {
		fm, _, err := parse(md)
		if err != nil {
			return nil, err
		}
		if fm.Name != s.Name {
			return nil, fmt.Errorf("%w: the name stays %s; upload the skill to add one under another name", ErrInvalid, s.Name)
		}
	} else if files[SkillFile], err = l.File(ctx, s, SkillFile); err != nil {
		return nil, err
	}
	return l.Save(ctx, Draft{Files: files, Meta: m, KeepFiles: true})
}

// Get fetches a skill by id.
func (l *Library) Get(ctx context.Context, id string) (*opensearch.Skill, error) {
	return l.records.Skill(ctx, id)
}

// Named fetches a skill by name; ErrNotFound when there is none.
func (l *Library) Named(ctx context.Context, name string) (*opensearch.Skill, error) {
	s, err := l.records.SkillNamed(ctx, name)
	if errors.Is(err, opensearch.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
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
	l.cleanUp(id, l.files.DeletePrefix(context.WithoutCancel(ctx), key(id, "")))
	return nil
}

// File reads one of a skill's files; ./references/x.md is references/x.md.
func (l *Library) File(ctx context.Context, s *opensearch.Skill, p string) ([]byte, error) {
	p = path.Clean(p)
	if !slices.ContainsFunc(s.Files, func(f opensearch.SkillFile) bool { return f.Path == p }) {
		return nil, fmt.Errorf("%w: %s has no %s", ErrFileNotFound, s.Name, p)
	}
	r, err := l.files.Get(ctx, key(s.ID, p))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// ContentType is what a skill's file is served as: by its extension, or by
// its content.
func ContentType(p string, b []byte) string {
	return cmp.Or(mime.TypeByExtension(path.Ext(p)), http.DetectContentType(b))
}

// key is where a skill's file is in S3.
func key(id, p string) string {
	return "skills/" + id + "/" + p
}
