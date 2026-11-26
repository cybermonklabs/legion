package specgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// GoCode is the Go code at one commit as the go command reads it: Dir is the module or workspace
// root that go commands run in, and Env holds "NAME=VALUE" entries that they get beyond the
// process's own environment, a later entry of one name winning. A GoCode keeps what go list and
// the parser said of each package that Find was asked of, so the checks of one package list and
// parse it once; its Find is not safe for concurrent use.
type GoCode struct {
	Dir   string
	Env   []string
	found map[string]pkgTests
}

// pkgTests is how many top-level tests of each name the test files of one package declare, the
// first test file by name that declares each name and that a build constraint leaves out of the
// package, or why they could not be read.
type pkgTests struct {
	count    map[string]int
	excluded map[string]string
	err      error
}

// ExcludedTestError is Find's error for a test that no file of its package that go test builds
// with no tags declares, and that a test file under a build constraint does declare: File, the
// first such file by name, which go list lists as IgnoredGoFiles, as for a //go:build e2e line or a
// _windows_test.go name. go list reads the default build context, so such a test is in the code
// that go test builds only with the tag or on the platform that the constraint names.
type ExcludedTestError struct {
	Name, File string
}

func (e *ExcludedTestError) Error() string {
	return fmt.Sprintf("specgraph: a build constraint leaves out %s, which declares %s", e.File, e.Name)
}

// Find is how many tests the test part t picks in the code (R48: "a check whose test is not in the
// code at that commit"): the top-level funcs, in the test files that go list lists for the
// selector's package, package p and package p_test both, that have the selector's test name and
// the signature that go test runs. 0 is a test that is not in the code, with an
// *ExcludedTestError when a test file that a build constraint leaves out declares it. A runner
// other than go, or a selector that ParseSelector refuses, is an error wrapping ErrSelector. A
// package that go list cannot list is an error too, a package that is gone included: go answers
// it as it answers a workspace that does not hold the module, so Find cannot say that the test is
// not in the code.
func (g *GoCode) Find(t Test) (int, error) {
	if t.Runner != "go" {
		return 0, fmt.Errorf("%w: runner %q", ErrSelector, t.Runner)
	}
	path, name, err := ParseSelector(t.Selector)
	if err != nil {
		return 0, err
	}
	p, asked := g.found[path]
	if !asked {
		p = g.tests(path)
		if g.found == nil {
			g.found = map[string]pkgTests{}
		}
		g.found[path] = p
	}
	if file, excluded := p.excluded[name]; excluded && p.err == nil && p.count[name] == 0 {
		return 0, &ExcludedTestError{Name: name, File: file}
	}
	return p.count[name], p.err
}

// tests is how many top-level funcs of each name, with the signature that go test runs, the test
// files that go list lists for the package at path declare, package p and package p_test both,
// and the first test file by name that declares each name among the files that a build
// constraint leaves out of the package (IgnoredGoFiles).
func (g *GoCode) tests(path string) pkgTests {
	// "--" ends go's flags, so a path that starts with "-" is a path that go refuses, never a flag.
	cmd := exec.Command("go", "list", "-json=Dir,TestGoFiles,XTestGoFiles,IgnoredGoFiles", "--", path)
	cmd.Dir = g.Dir
	// The environment that exec gives a command with no Env, as every other command of this
	// package runs, then g.Env: Find reads no variable of its own.
	cmd.Env = append(cmd.Environ(), g.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return pkgTests{err: fmt.Errorf("specgraph: go list %s in %s: %w: %s", path, g.Dir, err, strings.Join(strings.Fields(stderr.String()), " "))}
	}
	type listed struct {
		Dir                                       string
		TestGoFiles, XTestGoFiles, IgnoredGoFiles []string
	}
	var pkgs []listed
	for decoder := json.NewDecoder(bytes.NewReader(out)); ; {
		var p listed
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return pkgTests{err: fmt.Errorf("specgraph: go list %s in %s: %w", path, g.Dir, err)}
		}
		pkgs = append(pkgs, p)
	}
	// A path with "..." is a pattern of several packages, and a selector picks one test.
	if len(pkgs) != 1 {
		return pkgTests{err: fmt.Errorf("specgraph: go list %s in %s listed %d packages, not one", path, g.Dir, len(pkgs))}
	}
	fset := token.NewFileSet()
	// declared is each name of a test that the file declares.
	declared := func(file string) ([]string, error) {
		parsed, err := parser.ParseFile(fset, filepath.Join(pkgs[0].Dir, file), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("specgraph: the tests of %s: %w", path, err)
		}
		var names []string
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && goTestSignature(fn) {
				names = append(names, fn.Name.Name)
			}
		}
		return names, nil
	}
	p := pkgTests{count: map[string]int{}, excluded: map[string]string{}}
	for _, file := range append(pkgs[0].TestGoFiles, pkgs[0].XTestGoFiles...) {
		names, err := declared(file)
		if err != nil {
			return pkgTests{err: err}
		}
		for _, name := range names {
			p.count[name]++
		}
	}
	// go list lists IgnoredGoFiles in the order of their names, so the first file of a name is the
	// same on each run. Such a file is no file of the build, so one that does not parse names no
	// test and is passed over, where a test file of the build that does not parse is an error.
	for _, file := range pkgs[0].IgnoredGoFiles {
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		names, err := declared(file)
		if err != nil {
			continue
		}
		for _, name := range names {
			if _, held := p.excluded[name]; !held {
				p.excluded[name] = filepath.Join(pkgs[0].Dir, file)
			}
		}
	}
	return p
}

// goTestSignature is whether fn has the signature of a test that go test runs: one parameter, of
// type *T or *<package>.T, and no result. Like go test (isTestFunc in cmd/go/internal/load), it
// cannot see how package testing was imported, so it takes the T of any package.
func goTestSignature(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if (fn.Type.Results != nil && len(fn.Type.Results.List) > 0) || len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	pointer, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch typ := pointer.X.(type) {
	case *ast.Ident:
		return typ.Name == "T"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "T"
	}
	return false
}
