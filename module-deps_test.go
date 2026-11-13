package asset

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/weakpixel/asset"

// TestNoDependencies enforces the hard rule of the module: no require in
// go.mod, and every package of the module imports only the standard library
// and the module itself. Nested modules (directories with their own go.mod)
// would have their own dependencies and are skipped.
func TestNoDependencies(t *testing.T) {
	f, err := os.Open("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), "require") {
			t.Errorf("go.mod: the module must not require anything: %q", sc.Text())
		}
	}

	fset := token.NewFileSet()
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
				if name := d.Name(); name == "build" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == modulePath || strings.HasPrefix(p, modulePath+"/") {
				continue
			}
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q: the module uses the standard library only", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
