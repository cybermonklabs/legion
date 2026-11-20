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

// pkgTests is how many top-level tests of each name the test files of one package declare, or why
// they could not be read.
type pkgTests struct {
	count map[string]int
	err   error
}

// Find is how many tests the test part t picks in the code (R48: "a check whose test is not in the
// code at that commit"): the top-level funcs, in the test files that go list lists for the
// selector's package, package p and package p_test both, that have the selector's test name and
// the signature that go test runs. 0 is a test that is not in the code. A runner other than go, or
// a selector that ParseSelector refuses, is an error wrapping ErrSelector. A package that go list
// cannot list is an error too, a package that is gone included: go answers it as it answers a
// workspace that does not hold the module, so Find cannot say that the test is not in the code.
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
		p.count, p.err = g.tests(path)
		if g.found == nil {
			g.found = map[string]pkgTests{}
		}
		g.found[path] = p
	}
	return p.count[name], p.err
}

// tests is how many top-level funcs of each name, with the signature that go test runs, the test
// files that go list lists for the package at path declare: package p and package p_test both.
func (g *GoCode) tests(path string) (map[string]int, error) {
	// "--" ends go's flags, so a path that starts with "-" is a path that go refuses, never a flag.
	cmd := exec.Command("go", "list", "-json=Dir,TestGoFiles,XTestGoFiles", "--", path)
	cmd.Dir = g.Dir
	// The environment that exec gives a command with no Env, as every other command of this
	// package runs, then g.Env: Find reads no variable of its own.
	cmd.Env = append(cmd.Environ(), g.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("specgraph: go list %s in %s: %w: %s", path, g.Dir, err, strings.Join(strings.Fields(stderr.String()), " "))
	}
	type listed struct {
		Dir                       string
		TestGoFiles, XTestGoFiles []string
	}
	var pkgs []listed
	for decoder := json.NewDecoder(bytes.NewReader(out)); ; {
		var p listed
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("specgraph: go list %s in %s: %w", path, g.Dir, err)
		}
		pkgs = append(pkgs, p)
	}
	// A path with "..." is a pattern of several packages, and a selector picks one test.
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("specgraph: go list %s in %s listed %d packages, not one", path, g.Dir, len(pkgs))
	}
	fset := token.NewFileSet()
	count := map[string]int{}
	for _, testFile := range append(pkgs[0].TestGoFiles, pkgs[0].XTestGoFiles...) {
		file, err := parser.ParseFile(fset, filepath.Join(pkgs[0].Dir, testFile), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("specgraph: the tests of %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && goTestSignature(fn) {
				count[fn.Name.Name]++
			}
		}
	}
	return count, nil
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
