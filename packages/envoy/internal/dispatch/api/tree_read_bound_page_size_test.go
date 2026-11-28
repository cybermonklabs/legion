package api

// c-tree-read-bound: no flag, setting or environment variable of the server reaches the node limit
// of the tree read, because only newTestServer sets Deps.IssueTreePageSize. The check's median stage
// greps for an assignment of the field, which finds a plain `=` and a key of a literal; this search
// reads every Go file of packages/envoy outside the _test.go files as the parser does and finds a
// write of the field in every form Go writes a field in. TestTheTreeReadAnswersTheSkeletonWithinItsBounds,
// which the check's job stage runs, calls it first, and TestNoFileOutsideTheTestsWritesIssueTreePageSize
// runs it in every build of this package's tests.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// issueTreePageSizeField is the name of the server's node limit of the tree read, the field of Deps
// (api/server.go) that only newTestServer sets.
const issueTreePageSizeField = "IssueTreePageSize"

// issueTreePageSizeWrite is one write of the field: the file, as its path from the repository root,
// the line, and the form of the write.
type issueTreePageSizeWrite struct {
	file string
	line int
	form string
}

func (write issueTreePageSizeWrite) String() string {
	return fmt.Sprintf("%s:%d: %s", write.file, write.line, write.form)
}

// issueTreePageSizeWritesIn is each write of the field that the Go source src, the file named name,
// holds, in each form Go writes a field: an assignment, of any operator, among them each target of a
// tuple, of a range clause and of a select case; an increment or a decrement; its address taken,
// which a write through a pointer, and each function handed one (fmt.Sscan, flag.IntVar), needs; a
// key of a composite literal; a literal of a type named Deps that keys no field, which writes every
// field in order; and a string that names it, as reflection's FieldByName names a field it then
// sets. A read of the field, its declaration and a comment that names it are none of these. A file
// that holds neither the field's name nor the name Deps is not parsed, because it can write it in
// none of those forms; one that holds either and does not parse answers the parser's error.
func issueTreePageSizeWritesIn(name string, src []byte) ([]issueTreePageSizeWrite, error) {
	if !bytes.Contains(src, []byte(issueTreePageSizeField)) && !bytes.Contains(src, []byte("Deps")) {
		return nil, nil
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var writes []issueTreePageSizeWrite
	found := func(at token.Pos, form string) {
		writes = append(writes, issueTreePageSizeWrite{file: name, line: files.Position(at).Line, form: form})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			for _, target := range node.Lhs {
				if !namesIssueTreePageSize(target) {
					continue
				}
				form := "an assignment (" + node.Tok.String() + ")"
				if len(node.Lhs) > 1 {
					form = "a target of a tuple assignment (" + node.Tok.String() + ")"
				}
				found(target.Pos(), form)
			}
		case *ast.IncDecStmt:
			if namesIssueTreePageSize(node.X) {
				found(node.Pos(), "an increment or decrement ("+node.Tok.String()+")")
			}
		case *ast.RangeStmt:
			for _, target := range []ast.Expr{node.Key, node.Value} {
				if target != nil && namesIssueTreePageSize(target) {
					found(target.Pos(), "a target of a range clause ("+node.Tok.String()+")")
				}
			}
		case *ast.UnaryExpr:
			if node.Op == token.AND && namesIssueTreePageSize(node.X) {
				found(node.Pos(), "its address taken (&), which a write through a pointer needs")
			}
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok && key.Name == issueTreePageSizeField {
				found(node.Pos(), "a key of a composite literal")
			}
		case *ast.CompositeLit:
			if namesDeps(node.Type) && len(node.Elts) > 0 && !slices.ContainsFunc(node.Elts, func(element ast.Expr) bool {
				_, keyed := element.(*ast.KeyValueExpr)
				return keyed
			}) {
				found(node.Pos(), "a literal of Deps that keys no field, which writes every field in order")
			}
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(node.Value); err == nil && value == issueTreePageSizeField {
				found(node.Pos(), "a string naming the field, as reflection's FieldByName names a field it sets")
			}
		}
		return true
	})
	return writes, nil
}

// namesIssueTreePageSize reports whether expression, its parentheses taken off, selects the field.
func namesIssueTreePageSize(expression ast.Expr) bool {
	for {
		inner, ok := expression.(*ast.ParenExpr)
		if !ok {
			break
		}
		expression = inner.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == issueTreePageSizeField
}

// namesDeps reports whether the type of a composite literal is named Deps, in this package or
// selected from another.
func namesDeps(typ ast.Expr) bool {
	switch typ := typ.(type) {
	case *ast.Ident:
		return typ.Name == "Deps"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "Deps"
	}
	return false
}

// issueTreePageSizeWritesUnder is each write of the field in the Go files under root outside the
// _test.go files, every directory read, each file named shown joined with its path from root, and
// how many Go files it read. A file it cannot read, or that names the field or Deps and does not
// parse, answers an error naming it, since the search cannot then say it holds no write.
func issueTreePageSizeWritesUnder(root, shown string) ([]issueTreePageSizeWrite, int, error) {
	var writes []issueTreePageSizeWrite
	read := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(shown, relative))
		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		read++
		found, err := issueTreePageSizeWritesIn(name, src)
		if err != nil {
			return fmt.Errorf("parse %s, which names %s or Deps: %w", name, issueTreePageSizeField, err)
		}
		writes = append(writes, found...)
		return nil
	})
	return writes, read, err
}

// issueTreePageSizePlanted is a write of the field in each form issueTreePageSizeWritesIn finds, by
// that form: the body of a function of a planted file of its own, whose write stands on the line
// that ends `// the write`.
var issueTreePageSizePlanted = map[string]string{
	"assignment":               "deps.IssueTreePageSize = limit // the write",
	"assignment-parenthesized": "(deps.IssueTreePageSize) = limit // the write",
	"assignment-operator":      "deps.IssueTreePageSize += limit // the write",
	"increment":                "deps.IssueTreePageSize++ // the write",
	"decrement":                "deps.IssueTreePageSize-- // the write",
	"tuple":                    "limit, deps.IssueTreePageSize = 3, limit // the write",
	"range":                    "for deps.IssueTreePageSize = range limit { // the write\n\t}",
	"select":                   "select {\n\tcase deps.IssueTreePageSize = <-limits: // the write\n\t}",
	"pointer":                  "pointer := &deps.IssueTreePageSize // the write\n\t*pointer = limit",
	"function":                 "fmt.Sscan(os.Getenv(\"DISPATCH_ISSUE_TREE_PAGE_SIZE\"), &deps.IssueTreePageSize) // the write",
	"keyed-literal":            "*deps = api.Deps{IssueTreePageSize: limit} // the write",
	"unkeyed-literal":          "*deps = api.Deps{nil, limit} // the write",
	"reflection":               "reflect.ValueOf(deps).Elem().FieldByName(\"IssueTreePageSize\").SetInt(int64(limit)) // the write",
}

// issueTreePageSizeRead is a planted file that reads the field, declares it and names it in a
// comment, and writes it in none of those, so the search finds no write in it.
const issueTreePageSizeRead = `package planted

// Deps.IssueTreePageSize = 2 is a comment, and no write.
type Deps struct {
	IssueTreePageSize int
}

func read(deps *Deps, fallback int) int {
	limit := deps.IssueTreePageSize
	return cmp.Or(limit, deps.IssueTreePageSize, fallback)
}
`

// requireIssueTreePageSizeUnset holds every Go file of packages/envoy outside the _test.go files to
// no write of Deps.IssueTreePageSize, in any form issueTreePageSizeWritesIn finds, and fails naming
// file:line and the form of each it finds. Before that it runs the same search over planted files
// in a directory of the test's own: one for each form, alone, which it must find on the line the
// planted file marks and nowhere else; one that reads the field alone, where it must find nothing;
// and one _test.go file that writes it, which it must not read. So a search that matched nothing
// cannot read as clean.
func requireIssueTreePageSizeUnset(t *testing.T) {
	t.Helper()
	planted := t.TempDir()
	forms := slices.Sorted(maps.Keys(issueTreePageSizePlanted))
	want := map[string]int{}
	for _, form := range forms {
		source := "package planted\n\nfunc plant(deps *api.Deps, limit int, limits chan int) {\n\t" + issueTreePageSizePlanted[form] + "\n}\n"
		lines := strings.Split(source, "\n")
		line := slices.IndexFunc(lines, func(line string) bool { return strings.HasSuffix(line, "// the write") }) + 1
		name := "planted_" + strings.ReplaceAll(form, "-", "_") + ".go"
		want["planted/"+name] = line
		if err := os.WriteFile(filepath.Join(planted, name), []byte(source), 0o644); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	for name, source := range map[string]string{
		"planted_read.go":       issueTreePageSizeRead,
		"planted_write_test.go": "package planted\n\nfunc plant(deps *api.Deps) { deps.IssueTreePageSize = 2 }\n",
	} {
		if err := os.WriteFile(filepath.Join(planted, name), []byte(source), 0o644); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	found, read, err := issueTreePageSizeWritesUnder(planted, "planted")
	if err != nil {
		t.Fatalf("c-tree-read-bound: search the planted files for a write of Deps.IssueTreePageSize: %v", err)
	}
	got := map[string][]int{}
	for _, write := range found {
		got[write.file] = append(got[write.file], write.line)
	}
	for file, line := range want {
		if !slices.Equal(got[file], []int{line}) {
			t.Errorf("c-tree-read-bound: the search for a write of Deps.IssueTreePageSize finds it at the lines %v of %s, want the planted line %d alone",
				got[file], file, line)
		}
	}
	for file, lines := range got {
		if _, planted := want[file]; !planted {
			t.Errorf("c-tree-read-bound: the search for a write of Deps.IssueTreePageSize finds it at the lines %v of %s, which writes none or is a test", lines, file)
		}
	}
	if read != len(want)+1 {
		t.Errorf("c-tree-read-bound: the search for a write of Deps.IssueTreePageSize read %d planted Go files, want the %d outside the _test.go files", read, len(want)+1)
	}
	if t.Failed() {
		t.Fatalf("c-tree-read-bound: the search for a write of Deps.IssueTreePageSize misses a planted write, so it cannot hold packages/envoy to none")
	}

	module, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	writes, read, err := issueTreePageSizeWritesUnder(module, "packages/envoy")
	if err != nil {
		t.Fatalf("c-tree-read-bound: search packages/envoy for a write of Deps.IssueTreePageSize: %v", err)
	}
	for _, write := range writes {
		t.Errorf("c-tree-read-bound: a file outside _test.go sets Deps.IssueTreePageSize: %s", write)
	}
	if len(writes) == 0 {
		t.Logf("c-tree-read-bound: no file outside _test.go sets Deps.IssueTreePageSize: %d Go files of packages/envoy read, and the search found the write "+
			"of each of the %d planted forms on its line alone (%s)", read, len(forms), strings.Join(forms, ", "))
	}
}

// TestNoFileOutsideTheTestsWritesIssueTreePageSize is c-tree-read-bound's search for a write of
// Deps.IssueTreePageSize (requireIssueTreePageSizeUnset), in every build of this package's tests.
func TestNoFileOutsideTheTestsWritesIssueTreePageSize(t *testing.T) {
	requireIssueTreePageSizeUnset(t)
}
