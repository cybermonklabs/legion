// Package testtypes tells a test that type-checks one package's source where the compiler left
// the types of everything that package imports, so the test reads those from the build cache
// instead of type-checking the whole dependency tree from source as well. admit's held-reason
// tests read the record package's words that way, and supervise's ProcessEnds guard reads its
// own package's. CheckPackage then type-checks a package's own directory against whichever types
// the test's importer opens: the daemon's decision-senders test and supervise's outcome source
// test, ProcessEnds guard and sealed all ask it, so they choose the files they read by one rule.
//
// It is an ordinary package rather than a _test.go file so every test package can import it; Go
// cannot share test-only code across packages.
package testtypes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// lists is the go command's answer for each pattern asked of it in this process, each asked on
// first use.
var lists struct {
	mu        sync.Mutex
	byPattern map[string]func() (importer.Lookup, error)
}

// CompiledDependencies is the lookup of the types the compiler left for pattern and for each
// package it imports: what importer.ForCompiler(fileset, "gc", lookup) opens when a test
// type-checks pattern's own source. The go command reports the file of each from its build
// cache, building any the test binary's own build did not. A test reads those rather than
// type-checking the dependency tree from its source, which supervise's dependencies make tens of
// seconds of a test binary's run (the source read supervise's sealed makes, in its table_test.go,
// is the longest test in that package by itself); the record package imports supervise, so its
// tree holds all of that.
//
// pattern is an import path, which the go command resolves from the process's working
// directory: a test's is its package's own, inside this module. The go command is asked once per
// pattern for the life of the process however many tests ask, and every caller gets that one
// answer, or that one failure. It takes no *testing.T, so the caller decides how to fail.
func CompiledDependencies(pattern string) (importer.Lookup, error) {
	lists.mu.Lock()
	list, found := lists.byPattern[pattern]
	if !found {
		list = sync.OnceValues(func() (importer.Lookup, error) { return listExports(pattern) })
		if lists.byPattern == nil {
			lists.byPattern = make(map[string]func() (importer.Lookup, error))
		}
		lists.byPattern[pattern] = list
	}
	lists.mu.Unlock()
	return list()
}

// listExports asks the go command where the compiler left the types of pattern and of each
// package it imports, and answers the lookup that opens them by import path.
func listExports(pattern string) (importer.Lookup, error) {
	listed, err := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Export", pattern).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("ask the go command where the dependencies of %s were compiled: %w: %s", pattern, err, exit.Stderr)
		}
		return nil, fmt.Errorf("ask the go command where the dependencies of %s were compiled: %w", pattern, err)
	}
	exports := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(listed))
	for decoder.More() {
		var dependency struct{ ImportPath, Export string }
		if err := decoder.Decode(&dependency); err != nil {
			return nil, fmt.Errorf("read the go command's answer for %s: %w", pattern, err)
		}
		if dependency.Export != "" {
			exports[dependency.ImportPath] = dependency.Export
		}
	}
	return func(path string) (io.ReadCloser, error) {
		export, found := exports[path]
		if !found {
			return nil, fmt.Errorf("the go command reported no compiled types for %s", path)
		}
		return os.Open(export)
	}, nil
}

// Package is one package's non-test source as this build compiles it, type-checked
// (CheckPackage).
type Package struct {
	// Types is the package the source declares.
	Types *types.Package
	// Info is what the check recorded of every expression's type and value, every definition,
	// every use and every selection: what a test reads to tell which constant, field or function
	// a line names, rather than how the line spells it.
	Info *types.Info
	// Files is each file the build compiles, parsed, in the order go/build lists them, which is
	// the order of their names: a test that walks them reads them, and reports what it finds, in
	// the same order on every run. A test reads a file's name from its position, through the
	// FileSet the check was given.
	Files []*ast.File
}

// CheckPackage parses the non-test Go files in dir that this build compiles, chosen by their
// build constraints as the go command chooses them (go/build's ImportDir), and type-checks them
// as the package path against imports, which for a test that reads the compiler's types of the
// package's imports is importer.ForCompiler over CompiledDependencies. The files are parsed into
// fset, so a test reads their positions through it. Every test that type-checks a package from
// its directory asks this one function, so no two of them read a different set of files as the
// same package; admit's held-reason test parses the record package's files itself, because it
// type-checks them with a word planted in their text as well. Like CompiledDependencies, it takes
// no *testing.T, so the caller decides how to fail.
func CheckPackage(fset *token.FileSet, imports types.Importer, path, dir string) (*Package, error) {
	listed, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, fmt.Errorf("list the source of %s in %s: %w", path, dir, err)
	}
	checked := &Package{Files: make([]*ast.File, 0, len(listed.GoFiles)), Info: &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}}
	for _, name := range listed.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s of %s: %w", name, path, err)
		}
		checked.Files = append(checked.Files, file)
	}
	config := types.Config{Importer: imports}
	if checked.Types, err = config.Check(path, fset, checked.Files, checked.Info); err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, err)
	}
	return checked, nil
}
