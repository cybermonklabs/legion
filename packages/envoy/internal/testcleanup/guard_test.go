package testcleanup

//ci:reads packages/envoy/internal/dispatch/docs/** TestTheDocumentTestsRegisterEveryCleanupThroughAdd cleanupCalls walks ../../internal/dispatch/docs and parses every Go file under it
//ci:reads packages/envoy/internal/dispatch/store/storetest/** TestTheDocumentTestsRegisterEveryCleanupThroughAdd cleanupCalls walks ../../internal/dispatch/store/storetest and parses every Go file under it

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// guarded are the directories, from the module root, whose Go files register every cleanup through
// Add: the document service's tests and the helpers they share. A cleanup registered there with
// t.Cleanup is held by testing until the package's last parallel test ends, with all it captured,
// and in internal/dispatch/docs that took the race binary past the 8 GB envoy-go-race-docs allows
// it (docs/testing.md, "A test in internal/dispatch/api or internal/dispatch/docs runs beside the
// others"). Each directory is read with every directory below it.
var guarded = []string{
	"internal/dispatch/docs",
	"internal/dispatch/store/storetest",
}

// The document tests and their helpers call no Cleanup method: they register a cleanup with Add.
// The check reads their Go files as the parser does, test files and the helpers' other files
// alike, and fails on every call whose function is a selector named Cleanup, whatever it is called
// on (a *testing.T, a *testing.B, a testing.TB, or a type wrapping one), so a comment that says
// t.Cleanup in English is prose rather than an offence, and a type that defines a Cleanup method
// of its own is no offence either.
func TestTheDocumentTestsRegisterEveryCleanupThroughAdd(t *testing.T) {
	t.Parallel()
	var offences []string
	for _, dir := range guarded {
		offences = append(offences, cleanupCalls(t, "../..", dir)...)
	}
	if len(offences) > 0 {
		t.Fatalf("cleanups registered outside testcleanup.Add (%d):\n\t%s\n"+
			"register them with testcleanup.Add(t, f), which drops its reference to f once f has run; testing keeps "+
			"a finished parallel test's cleanups, and all they captured, until the package's last parallel test ends",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// The check finds a Cleanup call in a test file and in a helper's other file, and leaves Add, a
// method named Cleanup defined on a type, and the word in a comment alone. Without this, a check
// that read nothing would pass.
func TestTheCleanupCheckFindsWhatItLooksFor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"pkg/a_test.go": "package pkg\n\nimport \"testing\"\n\n" +
			"// t.Cleanup in a comment is prose.\n" +
			"func TestA(t *testing.T) {\n\tt.Cleanup(func() {})\n\ttestcleanup.Add(t, func() {})\n}\n",
		"pkg/helper.go": "package pkg\n\nimport \"testing\"\n\n" +
			"type wrap struct{ testing.TB }\n\n" +
			"func (w wrap) Cleanup(f func()) {}\n\n" +
			"func help(tb testing.TB) { tb.Cleanup(func() {}) }\n",
		"pkg/nested/b_test.go": "package nested\n\nimport \"testing\"\n\nfunc BenchmarkB(b *testing.B) { b.Cleanup(func() {}) }\n",
		"pkg/c.go":             "package pkg\n\nfunc none() {}\n",
	}
	for name, source := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := cleanupCalls(t, root, "pkg")
	want := []string{"pkg/a_test.go:7: t.Cleanup", "pkg/helper.go:9: tb.Cleanup", "pkg/nested/b_test.go:5: b.Cleanup"}
	if !slices.Equal(got, want) {
		t.Fatalf("the check found %q, want %q", got, want)
	}
}

// cleanupCalls names, as file:line and the call, every Cleanup call in the Go files under dir,
// written from root. A file whose bytes do not hold "Cleanup(" is not parsed: an offence is
// reported from a call whose selector is named Cleanup, copied from these same bytes.
func cleanupCalls(t *testing.T, root, dir string) []string {
	t.Helper()
	var offences []string
	read := 0
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		read++
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte("Cleanup(")) {
			return nil
		}
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, path, source, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Cleanup" {
				return true
			}
			receiver := "?"
			if ident, ok := selector.X.(*ast.Ident); ok {
				receiver = ident.Name
			}
			offences = append(offences, relative+":"+strconv.Itoa(files.Position(call.Pos()).Line)+": "+receiver+".Cleanup")
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	// A directory moved or emptied would leave nothing to read, and the check would pass on it.
	if read == 0 {
		t.Fatalf("read no Go file under %s: the check has nothing to guard", dir)
	}
	slices.Sort(offences)
	return offences
}
