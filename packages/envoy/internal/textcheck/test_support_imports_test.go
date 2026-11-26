package textcheck

//ci:reads packages/envoy/**/*.go !**/*_test.go !packages/envoy/**/testdata/** TestOnlyTestFilesImportSpecgraphtest calls nonTestImporters on ../.., whose walk passes over testdata, vendor and node_modules and parses each .go file but the test files

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// internal/dispatch/specgraph/specgraphtest is test support: the one conversion of a scenario's
// check to another kind, which the tests of specgraph and of the spec-check command share. It is an
// ordinary package so that the tests of two packages can import it, and only test files may import
// it: a file that a build of the module compiles into a program would carry a test's fixture into
// that program. The check reads the import declarations of every Go file of the module that is no
// test file, whatever build constraint the file carries, so a file of any tag that imports the
// package fails here, naming the file.
func TestOnlyTestFilesImportSpecgraphtest(t *testing.T) {
	const support = "github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
	importers, read := nonTestImporters(t, "../..", support)
	if read == 0 {
		t.Fatal("read no Go file that is no test file under ../..")
	}
	if len(importers) > 0 {
		t.Fatalf("%d of the %d Go files that are no test files import %s, which only test files import:\n\t%s",
			len(importers), read, support, strings.Join(importers, "\n\t"))
	}
	t.Logf("%d Go files that are no test files read under ../.., none of them an importer of %s", read, support)
}

// nonTestImporters is each Go file under root, a testdata folder's aside, that is no _test.go file
// and imports the package path, by its path, and the count of such files that it read.
func nonTestImporters(t *testing.T, root, path string) (importers []string, read int) {
	t.Helper()
	files := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(files, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		read++
		for _, spec := range file.Imports {
			if imported, err := strconv.Unquote(spec.Path.Value); err == nil && imported == path {
				importers = append(importers, filepath.ToSlash(p))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files: %v", err)
	}
	slices.Sort(importers)
	return importers, read
}
