package toolx

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyToolxBuildsFunctionTools keeps adk's functiontool out of every
// package but this one, so every tool goes through New.
func TestOnlyToolxBuildsFunctionTools(t *testing.T) {
	const functiontool = "google.golang.org/adk/v2/tool/functiontool"
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if dir, _ := filepath.Abs(filepath.Dir(path)); dir == here {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == functiontool {
				t.Errorf("%s imports %s; build tools with toolx.New", path, functiontool)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
