package record

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// copyWriterMethods are the methods of Store that write dispatch_issues: a reading's write, and
// the refill's empty of a project's rows at its first boot on the release of
// 20261120055717_dispatch_issue_value_sequences.
var copyWriterMethods = []string{"PutDispatchIssue", "TakeDispatchRefill"}

// copyWriter is one use of a method of copyWriterMethods in the daemon's source: the file it is
// in, relative to the daemon's module, the function it is in, and the method it names.
type copyWriter struct {
	file, function, method string
}

// Only a reading of Dispatch writes the copy of Dispatch's issues (dispatch://INFRA-3320/spec
// R16), so only the code that writes a reading, and the boot that refills the copy from its
// listing, names a method that writes it. The set below is every such use in the daemon's
// non-test source, read from that source: the mirror's one write (blockers.Mirror.put), which an
// event, a view, the release's read, the periodic turn's re-read and a listing all reach, and
// the boot's refill in reconcileListing, which empties the project's rows in the transaction that
// fills them from the listing. The replay of a recorded window (internal/replay) replays that
// refill at each boot the record holds, on the replay's own database, in the transaction that
// fills the rows from its copy of Dispatch's listing ((*driver).boot), and writes every reading
// through the mirror. A use added anywhere else is a member the set does not hold, and the test
// fails naming its file and function.
func TestEveryWriterOfTheCopyIsAReading(t *testing.T) {
	want := []copyWriter{
		{file: "internal/blockers/mirror.go", function: "(*Mirror).put", method: "PutDispatchIssue"},
		{file: "internal/daemon/workflow.go", function: "(*workflowRuntime).reconcileListing", method: "TakeDispatchRefill"},
		{file: "internal/replay/driver.go", function: "(*driver).boot", method: "TakeDispatchRefill"},
	}
	got := copyWriters(t, filepath.Join("..", ".."), "cmd", "internal")
	for _, writer := range got {
		if !slices.Contains(want, writer) {
			t.Errorf("%s names %s in %s: only a reading of Dispatch writes the copy, through blockers.Mirror.put, and only the boot's refill in reconcileListing empties it",
				writer.file, writer.method, writer.function)
		}
	}
	for _, writer := range want {
		if !slices.Contains(got, writer) {
			t.Errorf("%s names %s nowhere in %s, which the set of the copy's writers holds", writer.file, writer.method, writer.function)
		}
	}
}

// copyWriters is every use of a method of copyWriterMethods in the non-test Go source under each
// of dirs of module: each selector that names one, called or taken as a method value, with the
// function declaration it is in, or "a package-level declaration" outside every function. The
// declarations of the methods themselves, on Postgres and on the Store interface, name them with
// no selector and are not uses.
func copyWriters(t *testing.T, module string, dirs ...string) []copyWriter {
	t.Helper()
	var writers []copyWriter
	files := 0
	for _, dir := range dirs {
		files += eachDeclaration(t, module, dir, func(file, function string, decl ast.Decl) {
			ast.Inspect(decl, func(node ast.Node) bool {
				if sel, ok := node.(*ast.SelectorExpr); ok && slices.Contains(copyWriterMethods, sel.Sel.Name) {
					writers = append(writers, copyWriter{file: file, function: function, method: sel.Sel.Name})
				}
				return true
			})
		})
	}
	if files < 100 {
		t.Fatalf("read %d non-test Go files under %v of %s, want the daemon's whole source", files, dirs, module)
	}
	slices.SortFunc(writers, func(a, b copyWriter) int {
		return cmp.Or(cmp.Compare(a.file, b.file), cmp.Compare(a.function, b.function), cmp.Compare(a.method, b.method))
	})
	return writers
}

// eachDeclaration hands visit every top-level declaration of the non-test Go source under dir
// of module (a slash-separated path), with its file relative to module, slash-separated, and the
// function it is (functionName), or "a package-level declaration" for one outside every
// function. It answers how many files it read.
func eachDeclaration(t *testing.T, module, dir string, visit func(file, function string, decl ast.Decl)) int {
	t.Helper()
	files := 0
	if err := filepath.WalkDir(filepath.Join(module, filepath.FromSlash(dir)), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		files++
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(module, path)
		if err != nil {
			return err
		}
		for _, decl := range parsed.Decls {
			function := "a package-level declaration"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				function = functionName(fn)
			}
			visit(filepath.ToSlash(rel), function, decl)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the Go source under %s: %v", filepath.Join(module, dir), err)
	}
	return files
}

// functionName is fn as a reader names it: Name for a function, (*Recv).Name for a method on a
// pointer receiver and Recv.Name for one on a value, with a generic receiver's type parameters
// left out.
func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	star, pointer := receiver.(*ast.StarExpr)
	if pointer {
		receiver = star.X
	}
	switch generic := receiver.(type) {
	case *ast.IndexExpr:
		receiver = generic.X
	case *ast.IndexListExpr:
		receiver = generic.X
	}
	name := receiver.(*ast.Ident).Name
	if pointer {
		return "(*" + name + ")." + fn.Name.Name
	}
	return name + "." + fn.Name.Name
}
