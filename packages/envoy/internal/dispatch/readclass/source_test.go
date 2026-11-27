package readclass

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// serverSource is the Dispatch server's source as the type checker reads it: every package of
// this module that cmd/dispatch builds in, and every package under internal/dispatch, each
// type-checked from its own non-test files, with every import read from its export data.
type serverSource struct {
	root     string
	fset     *token.FileSet
	imports  types.Importer
	packages []*checkedPackage
}

// checkedPackage is one type-checked package: its import path, its directory relative to the
// module root, its files and what the checker resolved in them.
type checkedPackage struct {
	path  string
	dir   string
	files []*ast.File
	info  *types.Info
}

// listedPackage is the part of `go list -json` the loader reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Module     *struct{ Main bool }
}

// sharedServerSource is the source TestMetricsNameNoProject reads, loaded once per test binary:
// loading runs go list, which builds every dependency's export data, and type-checks the server.
var sharedServerSource = sync.OnceValues(loadServerSource)

func loadedServerSource(t *testing.T) *serverSource {
	t.Helper()
	source, err := sharedServerSource()
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// loadServerSource reads the packages from its own go list and the files on disk, so a control
// that edits a file of the server is read as it stands.
func loadServerSource() (*serverSource, error) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		return nil, err
	}
	command := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export,Module",
		"./cmd/dispatch", "./internal/dispatch/...")
	command.Dir = root
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list the Dispatch server's packages: %w\n%s", err, stderr.String())
	}
	var listed []listedPackage
	exports := map[string]string{}
	for decoder := json.NewDecoder(bytes.NewReader(output)); ; {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list: %w", err)
		}
		listed = append(listed, pkg)
		exports[pkg.ImportPath] = pkg.Export
	}
	fset := token.NewFileSet()
	source := &serverSource{
		root: root,
		fset: fset,
		imports: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			if exports[path] == "" {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(exports[path])
		}),
	}
	for _, pkg := range listed {
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		var paths []string
		for _, name := range pkg.GoFiles {
			paths = append(paths, filepath.Join(pkg.Dir, name))
		}
		checked, err := source.check(pkg.ImportPath, pkg.Dir, paths)
		if err != nil {
			return nil, err
		}
		source.packages = append(source.packages, checked)
	}
	if len(source.packages) == 0 {
		return nil, errors.New("go list named no package of this module, so there is no source to read")
	}
	return source, nil
}

// check parses and type-checks one package's files. A file the checker cannot type is an error:
// a walk over a package it half read would pass over what it could not see.
func (s *serverSource) check(path, dir string, files []string) (*checkedPackage, error) {
	relative, err := filepath.Rel(s.root, dir)
	if err != nil {
		return nil, err
	}
	checked := &checkedPackage{path: path, dir: filepath.ToSlash(relative), info: &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}}
	for _, name := range files {
		file, err := parser.ParseFile(s.fset, name, nil, 0)
		if err != nil {
			return nil, err
		}
		checked.files = append(checked.files, file)
	}
	var problems []error
	config := types.Config{Importer: s.imports, Error: func(err error) { problems = append(problems, err) }}
	if _, err := config.Check(path, s.fset, checked.files, checked.info); len(problems) > 0 || err != nil {
		return nil, fmt.Errorf("type-check %s: %w", path, errors.Join(append(problems, err)...))
	}
	return checked, nil
}

// fixture type-checks the package in testdata/<name>, a tree no go list reads, with the server's
// own imports.
func (s *serverSource) fixture(t *testing.T, name string) *checkedPackage {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", name, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the fixture testdata/%s holds no Go file (%v)", name, err)
	}
	dir, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	checked, err := s.check("fixture/"+name, dir, files)
	if err != nil {
		t.Fatal(err)
	}
	return checked
}

// position is where node stands, relative to the module root.
func (s *serverSource) position(node ast.Node) string {
	position := s.fset.Position(node.Pos())
	if relative, err := filepath.Rel(s.root, position.Filename); err == nil {
		position.Filename = filepath.ToSlash(relative)
	}
	return position.String()
}

// calledFunc is the function or method a call names, as the checker resolved it, or nil for a
// call of a value, a conversion or a builtin.
func (p *checkedPackage) calledFunc(call *ast.CallExpr) *types.Func {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.SelectorExpr:
		if selection, ok := p.info.Selections[fun]; ok {
			called, _ := selection.Obj().(*types.Func)
			return called
		}
		called, _ := p.info.Uses[fun.Sel].(*types.Func)
		return called
	case *ast.Ident:
		called, _ := p.info.Uses[fun].(*types.Func)
		return called
	}
	return nil
}
