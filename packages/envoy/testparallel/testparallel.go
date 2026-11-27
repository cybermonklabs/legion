// Package testparallel holds the rule of a package whose tests run beside each other, in either
// Go module of the repository (docs/testing.md, "A test in a package whose tests run beside each
// other"): each top-level test of the package calls t.Parallel() as its first statement, or gives
// the reason it cannot in a `// Not parallel:` comment on its first line. The reason names the
// call or the bound that makes the test serial, never a line of a file, which the next edit above
// that line makes false. A test that does neither runs alone, before every parallel test, for a
// reason nobody wrote down, and a lane that looks for the cause of a failure of its own then
// spends a round ruling it out. Require is the rule's check, which each such package runs from a
// test of its own.
//
// The package is not internal, so the daemon module's packages, which require this module, call
// the same check.
package testparallel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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
	citesALine   = "names a line in its `// Not parallel:` comment, which the next edit above that line makes false: name the call or the bound"
)

// citedLine is a line a comment names: a file's (logtest.go:36), a bare one (:85, after a space,
// a line's start or a parenthesis, so that an address's port such as 127.0.0.1:1 and a time such
// as 10:30 are not read as one), or one in words (line 85, lines 85-90, with any space between, a
// line break included). It is matched against the comment's text with each line's // dropped
// (ast.CommentGroup.Text), so a citation on the comment's second line counts as one on its first.
var citedLine = regexp.MustCompile(`(?im)\.go:\d+|(?:^|[\s(]):\d+\b|\blines?\s+\d+`)

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
			"call t.Parallel() first, or write on the test's first line `// Not parallel:` and the call or bound that keeps it from running beside the others, never a line (docs/testing.md)",
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
// file order. It reads every _test.go file of dir, whatever its build constraint: a file behind a
// tag such as e2e or multiprocess still runs in some lane, and its tests run beside each other
// there.
func serialTests(dir string) (read int, broken, marked []foundTest, err error) {
	// A directory that is not there globs to nothing, which would read as a package with no test.
	if _, err := os.Stat(dir); err != nil {
		return 0, nil, nil, err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return 0, nil, nil, err
	}
	fset := token.NewFileSet()
	for _, path := range names {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return 0, nil, nil, err
		}
		name := filepath.Base(path)
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
			reason, says, text := notParallelReason(file, fn.Body)
			switch {
			case says && callsParallel(param, fn.Body):
				at.note = contradicted
				broken = append(broken, at)
			case callsParallelFirst(param, fn.Body):
			case reason == "":
				at.note = forgetsBoth
				broken = append(broken, at)
			case citedLine.MatchString(text):
				at.note = citesALine + " (" + strings.TrimSpace(citedLine.FindString(text)) + ")"
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
// captures param is read as the test's own code. The branches of an if statement are passed over
// too, and its condition is read: a test that runs itself again in a run of the test binary of its
// own (internal/alonetest) calls t.Parallel() only in the branch of the run that starts the other,
// which measures nothing and only waits, and its `// Not parallel:` comment gives the reason of the
// run alone, which cannot run beside anything.
func callsParallel(param *ast.Ident, body *ast.BlockStmt) bool {
	if param == nil {
		return false
	}
	found := false
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		// ast.Inspect visits a node's later siblings whatever this returns: once found, this guard
		// ends the walk's work there.
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return !declares(n.Type, param.Name)
		case *ast.IfStmt:
			if n.Init != nil {
				ast.Inspect(n.Init, visit)
			}
			ast.Inspect(n.Cond, visit)
			return false
		case *ast.SelectorExpr:
			if isParallelOf(param, n) {
				found = true
			}
		}
		return true
	}
	ast.Inspect(body, visit)
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
// that comment's first line opens with `Not parallel:`, reason is the rest of that line, "" where
// the line gives none, and text is the whole comment group with each line's // dropped
// (ast.CommentGroup.Text), which citedLine reads. The first line is read as written
// (ast.Comment.Text), since ast.CommentGroup.Text drops a group's empty leading lines and would
// read a reason under an empty `//` as the first line's.
func notParallelReason(file *ast.File, body *ast.BlockStmt) (reason string, says bool, text string) {
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
				return "", false, ""
			}
			return strings.TrimSpace(strings.TrimSuffix(rest, "*/")), true, group.Text()
		}
	}
	return "", false, ""
}
