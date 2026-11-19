//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
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
	"sort"
)

type listed struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Module     *struct{ Path string }
}

// load reads the packages that hold files (named below moduleDir, a directory of root) from
// source, the planted file among the first named file's package, type-checked together in the
// order `go list -deps` gives, with every other package from its export data. It answers the
// analysis ready to run, every file it read, and the first type errors.
func load(root, moduleDir string, names []string) (*analysis, []*ast.File, []string) {
	module := filepath.Join(root, moduleDir)
	report := map[string]bool{}
	packageDirs := map[string]bool{}
	var firstDir string
	for _, file := range names {
		abs := filepath.Join(module, file)
		rel, _ := filepath.Rel(root, abs)
		report[rel] = true
		packageDirs[filepath.Dir(abs)] = true
		if firstDir == "" {
			firstDir = filepath.Dir(abs)
		}
	}
	var dirs []string
	for dir := range packageDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	list := exec.Command("go", append([]string{"list", "-deps", "-export", "-json=ImportPath,Dir,GoFiles,Export,Module"}, dirs...)...)
	list.Dir = module
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		fail(fmt.Errorf("go list: %w", err))
	}
	var packages []listed
	decoder := json.NewDecoder(bytes.NewReader(out))
	for decoder.More() {
		var p listed
		if err := decoder.Decode(&p); err != nil {
			fail(err)
		}
		packages = append(packages, p)
	}
	modulePath := ""
	for _, p := range packages {
		if p.Dir == firstDir && p.Module != nil {
			modulePath = p.Module.Path
		}
	}
	if modulePath == "" {
		fail(fmt.Errorf("no package of %s is in a module", firstDir))
	}
	fset := token.NewFileSet()
	exports := map[string]string{}
	for _, p := range packages {
		if p.Export != "" {
			exports[p.ImportPath] = p.Export
		}
	}
	gc := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	a := &analysis{
		root: root, fset: fset, text: map[string][]byte{}, bodies: map[*types.Func]*ast.FuncDecl{},
		ids: map[string]int{}, carried: map[any]set{}, report: report, read: map[string]bool{},
		sites: map[*types.Var]map[token.Pos]*site{}, module: modulePath, urls: map[int]bool{},
		info: &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	plantedPath := filepath.Join(root, plantedName)
	checked := map[string]*types.Package{}
	var files []*ast.File
	var typeErrors []string
	for _, p := range packages {
		if !packageDirs[p.Dir] {
			continue
		}
		var pkgFiles []*ast.File
		for _, name := range p.GoFiles {
			path := filepath.Join(p.Dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				fail(err)
			}
			f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
			if err != nil {
				fail(err)
			}
			a.text[path] = src
			// Every file of a package it reads is reported, not only the named one: a refusal
			// written in one file of a package is printed by another.
			if rel, err := filepath.Rel(root, path); err == nil {
				report[rel] = true
			}
			pkgFiles = append(pkgFiles, f)
		}
		if p.Dir == firstDir {
			f, err := parser.ParseFile(fset, plantedPath, planted, parser.ParseComments)
			if err != nil {
				fail(err)
			}
			a.text[plantedPath] = []byte(planted)
			pkgFiles = append(pkgFiles, f)
			report[plantedName] = true
		}
		conf := types.Config{
			Importer: importerFunc(func(path string) (*types.Package, error) {
				if pkg := checked[path]; pkg != nil {
					return pkg, nil
				}
				return gc.Import(path)
			}),
			Error: func(err error) {
				if len(typeErrors) < 20 {
					typeErrors = append(typeErrors, err.Error())
				}
			},
		}
		pkg, _ := conf.Check(p.ImportPath, fset, pkgFiles, a.info)
		checked[p.ImportPath] = pkg
		a.read[p.ImportPath] = true
		files = append(files, pkgFiles...)
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				a.lits = append(a.lits, span{x.Pos(), x.End()})
			case *ast.BlockStmt:
				a.blocks = append(a.blocks, span{x.Pos(), x.End()})
			case *ast.CaseClause:
				a.blocks = append(a.blocks, span{x.Colon, x.End()})
			case *ast.CommClause:
				a.blocks = append(a.blocks, span{x.Colon, x.End()})
			}
			return true
		})
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				if fn, ok := a.info.Defs[fd.Name].(*types.Func); ok {
					a.bodies[fn] = fd
				}
			}
		}
	}
	return a, files, typeErrors
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "setting-prints:", err)
	os.Exit(1)
}
