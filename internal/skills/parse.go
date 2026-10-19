package skills

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/adrg/frontmatter"
	"github.com/goccy/go-yaml"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// SkillFile is the file that defines a skill.
const SkillFile = "SKILL.md"

// Limits on a skill, as npx skills has them: files across the whole skill,
// bytes unzipped per upload. The upload route's body limit bounds the zip
// itself.
const (
	maxFiles = 1000
	MaxBytes = 25 << 20
)

// ErrInvalid is a skill that breaks the spec or the limits.
var ErrInvalid = errors.New("invalid skill")

var yamlFrontmatter = frontmatter.NewFormat("---", "---", yaml.Unmarshal)

// Read reads an uploaded skill's files, by path: a zip with SKILL.md at its
// root or in its one top folder, or SKILL.md alone. Hidden files and macOS's
// __MACOSX are left out. archive/zip refuses a zip with a path that leaves
// it (go.mod sets zipinsecurepath=0) and reads no file past its stated size,
// and the sizes are checked as stated before anything is read, so a zip bomb
// stops at the limit.
func Read(data []byte) (map[string][]byte, error) {
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return map[string][]byte{SkillFile: data}, nil
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
		if total += info.Size(); total > MaxBytes {
			return fmt.Errorf("%w: more than %d MiB", ErrInvalid, MaxBytes>>20)
		}
		files[p], err = fs.ReadFile(root, p)
		return err
	})
	if err != nil && !errors.Is(err, ErrInvalid) {
		err = fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err != nil {
		return nil, err
	}
	return files, nil
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

// check checks a skill's files, by path, and returns its frontmatter: they
// must be within the limits, every path a plain relative one as an S3 key
// and a URL take it, and SKILL.md must meet the spec.
func check(files map[string][]byte) (*skill.Frontmatter, error) {
	if len(files) > maxFiles {
		return nil, fmt.Errorf("%w: more than %d files", ErrInvalid, maxFiles)
	}
	total := 0
	for p, b := range files {
		dots := strings.Contains(p, "..") // npx skills refuses an index with ".." in any path
		if !fs.ValidPath(p) || p == "." || dots {
			return nil, fmt.Errorf("%w: path %q", ErrInvalid, p)
		}
		total += len(b)
	}
	if total > MaxBytes {
		return nil, fmt.Errorf("%w: more than %d MiB", ErrInvalid, MaxBytes>>20)
	}
	fm, _, err := parse(files[SkillFile])
	return fm, err
}

// parse reads a SKILL.md: its frontmatter, checked with adk's skill.Validate,
// and its body. It is lenient where adk's parser is not: fields the spec does
// not have, such as Claude Code's, are allowed.
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
