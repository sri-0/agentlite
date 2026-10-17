// Package skills is the skills library: Agent Skills, each a SKILL.md and any
// other files (https://agentskills.io/specification), kept in OpenSearch,
// given to agents through adk's skill toolset and served to npx skills.
package skills

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/adrg/frontmatter"
	"github.com/goccy/go-yaml"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"agentlite/pkg/db/opensearch"
)

// SkillFile is the file that defines a skill.
const SkillFile = "SKILL.md"

// Limits on an uploaded skill, as npx skills has them.
const (
	maxFiles = 1000
	maxBytes = 25 << 20
)

// ErrInvalid is a skill that breaks the spec or the limits.
var ErrInvalid = errors.New("invalid skill")

var yamlFrontmatter = frontmatter.NewFormat("---", "---", yaml.Unmarshal)

// Read reads an uploaded skill: a zip with SKILL.md at its root or in its one
// top folder, or SKILL.md alone. Hidden files and macOS's __MACOSX are left
// out.
func Read(data []byte) (*opensearch.Skill, error) {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return New(map[string][]byte{SkillFile: data})
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	root, err := skillRoot(zr)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	var total int64
	err = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		hidden := strings.HasPrefix(d.Name(), ".") || d.Name() == "__MACOSX" // .DS_Store, ._name, Finder's __MACOSX
		switch {
		case p != "." && hidden:
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%w: %s is not a regular file", ErrInvalid, p)
		case len(files) == maxFiles:
			return fmt.Errorf("%w: more than %d files", ErrInvalid, maxFiles)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if total += info.Size(); total > maxBytes { // archive/zip refuses to read past a file's stated size
			return fmt.Errorf("%w: more than %d MiB", ErrInvalid, maxBytes>>20)
		}
		files[p], err = fs.ReadFile(root, p)
		return err
	})
	if err != nil {
		if !errors.Is(err, ErrInvalid) {
			err = fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return nil, err
	}
	return New(files)
}

// skillRoot is the folder of a zip that holds SKILL.md: its root, or its one
// top folder, as a zip of the skill's folder has.
func skillRoot(zr *zip.Reader) (fs.FS, error) {
	if _, err := fs.Stat(zr, SkillFile); err == nil {
		return zr, nil
	}
	if m, _ := fs.Glob(zr, "*/"+SkillFile); len(m) == 1 {
		return fs.Sub(zr, path.Dir(m[0]))
	}
	return nil, fmt.Errorf("%w: the zip needs %s at its root, or exactly one top folder with it", ErrInvalid, SkillFile)
}

// New builds a skill from its files, keyed by path. Its SKILL.md's frontmatter
// must meet the spec (adk's skill.Validate); fields the spec does not have,
// such as Claude Code's, are allowed.
func New(files map[string][]byte) (*opensearch.Skill, error) {
	fm, _, err := parse(files[SkillFile])
	if err != nil {
		return nil, err
	}
	s := &opensearch.Skill{Name: fm.Name, Description: fm.Description}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		dots := strings.Contains(p, "..") // npx skills refuses an index with ".." in any path
		if !fs.ValidPath(p) || p == "." || dots {
			return nil, fmt.Errorf("%w: path %q", ErrInvalid, p)
		}
		s.Files = append(s.Files, opensearch.SkillFile{Path: p, Content: files[p]})
	}
	return s, nil
}

// parse reads a SKILL.md: its frontmatter, checked, and its body.
func parse(md []byte) (*skill.Frontmatter, string, error) {
	if md == nil {
		return nil, "", fmt.Errorf("%w: no %s", ErrInvalid, SkillFile)
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	body, err := frontmatter.MustParse(bytes.NewReader(md), &fm, yamlFrontmatter)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s frontmatter: %v", ErrInvalid, SkillFile, err)
	}
	out := &skill.Frontmatter{Name: fm.Name, Description: fm.Description}
	if err := skill.Validate(out); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return out, strings.TrimSpace(string(body)), nil
}
