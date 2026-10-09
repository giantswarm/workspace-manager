package kinds

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const module = "github.com/giantswarm/workspace-manager"

// notKinds are the packages below internal/provider that are not a kind.
var notKinds = map[string]bool{"kinds": true, "providertest": true}

// TestKindsStayInTheirPackage checks that no package outside
// internal/provider/<kind> imports a kind's implementation, but for this
// package, the one list of compiled-in kinds; and that the fake kind is
// imported by tests alone.
func TestKindsStayInTheirPackage(t *testing.T) {
	root, err := filepath.Abs("../../..")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "module root")

	entries, err := os.ReadDir(filepath.Join(root, "internal", "provider"))
	require.NoError(t, err)
	var kindDirs []string
	for _, e := range entries {
		if e.IsDir() && !notKinds[e.Name()] {
			kindDirs = append(kindDirs, e.Name())
		}
	}
	require.Contains(t, kindDirs, "github")
	require.Contains(t, kindDirs, "fake")

	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, kind := range kindDirs {
				dir := "internal/provider/" + kind
				if p != module+"/"+dir && !strings.HasPrefix(p, module+"/"+dir+"/") {
					continue
				}
				inKind := strings.HasPrefix(rel, dir+"/")
				inKinds := strings.HasPrefix(rel, "internal/provider/kinds/")
				assert.True(t, inKind || inKinds, "%s imports the %s kind's %s: only internal/provider/kinds may", rel, kind, p)
				if kind == "fake" && !inKind {
					assert.True(t, strings.HasSuffix(rel, "_test.go"), "%s imports the fake kind outside a test", rel)
				}
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 10, "the walk saw the module's files")
}
