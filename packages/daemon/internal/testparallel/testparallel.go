// Package testparallel holds the rule of a package whose tests run beside each other: each
// top-level test of the package's default build calls t.Parallel() as its first statement, or
// gives the reason it cannot in a `// Not parallel:` comment on its first line. A test that does
// neither runs alone, before every parallel test, for a reason nobody wrote down, and a lane that
// looks for the cause of a failure of its own then spends a round ruling it out. Require is the
// rule's check, which such a package runs from a test of its own (docs/testing.md names them).
package testparallel

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// foundTest is a test the check names: the file it is declared in, its line and its name, and a
// note, which says how it breaks the rule or, for a test that keeps it by running alone, gives the
// reason its first line gives.
type foundTest struct {
	file string
	line int
	name string
	note string
}

// at is where the test is declared, and its name.
func (f foundTest) at() string {
	return f.file + ":" + strconv.Itoa(f.line) + " " + f.name
}

const (
	forgetsBoth  = "neither calls t.Parallel() as its first statement nor gives a reason in a `// Not parallel:` comment on its first line"
	contradicted = "calls t.Parallel() under a `// Not parallel:` comment"
)

// Require fails t, naming each top-level test of the package in dir that breaks the rule
// (serialTests), and logs each test that runs alone with the reason its first line gives. It also
// fails t when it cannot read the package's test files, and when it reads no test there, since a
// check that read nothing passes nothing.
func Require(t testing.TB, dir string) {
	t.Helper()
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	read, broken, marked, err := serialTests(dir)
	switch {
	case err != nil:
		t.Errorf("read the tests of %s: %v", dir, err)
		return
	case read == 0:
		t.Errorf("read no test in %s: the reading itself is broken, and a check that read nothing passes nothing", dir)
		return
	}
	if len(marked) > 0 {
		t.Logf("%d of the %d tests in %s run alone, each for the reason on its first line:\n\t%s", len(marked), read, dir,
			joined(marked, func(f foundTest) string { return f.at() + " (" + f.note + ")" }))
	}
	if len(broken) > 0 {
		t.Errorf("%d of the %d tests in %s break the rule of a package whose tests run beside each other:\n\t%s\n"+
			"call t.Parallel() first, or write on the test's first line `// Not parallel:` and the reason it cannot run beside the others (docs/testing.md)",
			len(broken), read, dir, joined(broken, func(f foundTest) string { return f.at() + " " + f.note }))
	}
}

// joined is each of tests as line writes it, one to a line.
func joined(tests []foundTest, line func(foundTest) string) string {
	lines := make([]string, len(tests))
	for i, f := range tests {
		lines[i] = line(f)
	}
	return strings.Join(lines, "\n\t")
}

// serialTests answers how many top-level tests the package in dir holds, those that break the
// rule, and those that keep it by running alone for the reason their first line gives, each in
// file order. It reads the test files go/build's default context selects: this GOOS and GOARCH and
// no -tags, so a file behind a tag such as e2e is not read.
func serialTests(dir string) (read int, broken, marked []foundTest, err error) {
	pkg, err := build.Default.ImportDir(dir, 0)
	if err != nil {
		return 0, nil, nil, err
	}
	fset := token.NewFileSet()
	for _, name := range slices.Concat(pkg.TestGoFiles, pkg.XTestGoFiles) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			param, ok := testParam(fn)
			if !ok {
				continue
			}
			read++
			at := foundTest{file: name, line: fset.Position(fn.Pos()).Line, name: fn.Name.Name}
			reason, says := notParallelReason(file, fn.Body)
			switch {
			case says && callsParallel(param, fn.Body):
				at.note = contradicted
				broken = append(broken, at)
			case callsParallelFirst(param, fn.Body):
			case reason == "":
				at.note = forgetsBoth
				broken = append(broken, at)
			default:
				at.note = reason
				marked = append(marked, at)
			}
		}
	}
	return read, broken, marked, nil
}

// testParam answers whether fn is a function go test runs as a test, by go test's own rule
// (cmd/go's isTestFunc): TestXxx, where Xxx does not start with a lower-case letter, with no
// result and one parameter of type *T or *<name>.T, since how a file imported testing (as
// testing, under another name or with a dot) the declaration alone does not say. It also answers
// that parameter's name, nil where the parameter has none.
func testParam(fn *ast.FuncDecl) (*ast.Ident, bool) {
	rest, ok := strings.CutPrefix(fn.Name.Name, "Test")
	if !ok || fn.Recv != nil || fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		return nil, false
	}
	if first, _ := utf8.DecodeRuneInString(rest); unicode.IsLower(first) {
		return nil, false
	}
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return nil, false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return nil, false
	}
	switch named := star.X.(type) {
	case *ast.Ident:
		ok = named.Name == "T"
	case *ast.SelectorExpr:
		ok = named.Sel.Name == "T"
	default:
		ok = false
	}
	if !ok || len(params[0].Names) == 0 {
		return nil, ok
	}
	return params[0].Names[0], true
}

// callsParallelFirst is whether the first statement of body calls Parallel on param, the test's
// own *testing.T, parenthesised or not.
func callsParallelFirst(param *ast.Ident, body *ast.BlockStmt) bool {
	if param == nil || len(body.List) == 0 {
		return false
	}
	statement, ok := body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := ast.Unparen(statement.X).(*ast.CallExpr)
	return ok && isParallelOf(param, call.Fun)
}

// callsParallel is whether body, at any depth, names Parallel on param: a call of it, or the
// method value taken to call later (p := t.Parallel). A function literal that declares a parameter
// of param's name, as a subtest's func(t *testing.T) does, is passed over, since that name is
// another *testing.T there and a subtest may run in parallel under a serial test; one that
// captures param is read as the test's own code.
func callsParallel(param *ast.Ident, body *ast.BlockStmt) bool {
	if param == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		// ast.Inspect visits a node's later siblings whatever this returns: once found, this guard
		// ends the walk's work there.
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return !declares(n.Type, param.Name)
		case *ast.SelectorExpr:
			if isParallelOf(param, n) {
				found = true
			}
		}
		return true
	})
	return found
}

// isParallelOf is whether expr is the selector Parallel on param, parenthesised or not.
func isParallelOf(param *ast.Ident, expr ast.Expr) bool {
	selector, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Parallel" {
		return false
	}
	receiver, ok := ast.Unparen(selector.X).(*ast.Ident)
	return ok && receiver.Name == param.Name
}

// declares is whether fn declares a parameter named name.
func declares(fn *ast.FuncType, name string) bool {
	for _, field := range fn.Params.List {
		for _, n := range field.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

// notParallelReason reads the first comment of body, before its first statement: says is whether
// that comment's first line opens with `Not parallel:`, and reason is the rest of that line, ""
// where the line gives none. The line is read as written (ast.Comment.Text), since
// ast.CommentGroup.Text drops a group's empty leading lines and would read a reason under an
// empty `//` as the first line's.
func notParallelReason(file *ast.File, body *ast.BlockStmt) (reason string, says bool) {
	end := body.Rbrace
	if len(body.List) > 0 {
		end = body.List[0].Pos()
	}
	for _, group := range file.Comments {
		if group.Pos() >= end {
			break
		}
		if group.Pos() > body.Lbrace {
			line, _, _ := strings.Cut(group.List[0].Text, "\n")
			line = strings.TrimPrefix(strings.TrimPrefix(line, "//"), "/*")
			rest, opens := strings.CutPrefix(strings.TrimSpace(line), "Not parallel:")
			if !opens {
				return "", false
			}
			return strings.TrimSpace(strings.TrimSuffix(rest, "*/")), true
		}
	}
	return "", false
}
