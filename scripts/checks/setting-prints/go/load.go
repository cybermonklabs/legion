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
	"strconv"
	"strings"
)

type listed struct {
	ImportPath string
	Name       string
	Dir        string
	GoFiles    []string
	Imports    []string
	Export     string
	Module     *struct{ Path, Dir string }
	Error      *struct{ Err string }
}

// pkg is one package of the module as the search reads it: its files (its planted ones among
// them), whether it is a command, and the packages of the module it imports.
type pkg struct {
	path    string
	command bool
	imports []string
	files   []*ast.File
}

// load reads every package of the module at moduleDir (a directory of root) from source, each
// planted file among its package's, type-checked together in the order `go list -deps` gives,
// with every package outside the module from its export data. It answers the analysis ready to
// run, the module's packages in that order, and what it could not load: a package go list or the
// type check refuses, named, and a planted file whose package the module does not hold. A package
// of the module it did not read would be a function whose text it cannot follow, so each such
// refusal is one the run must report, never pass over.
func load(root, moduleDir string) (*analysis, []*pkg, []string) {
	module := filepath.Join(root, moduleDir)
	list := exec.Command("go", "list", "-e", "-deps", "-export", "-json=ImportPath,Name,Dir,GoFiles,Imports,Export,Module,Error", "./...")
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
		if p.Module != nil && p.Module.Dir == module {
			modulePath = p.Module.Path
		}
	}
	if modulePath == "" {
		fail(fmt.Errorf("no package of %s is in a module", module))
	}
	ofModule := func(p listed) bool { return p.Module != nil && p.Module.Path == modulePath }
	inModule := map[string]bool{}
	for _, p := range packages {
		if ofModule(p) {
			inModule[p.ImportPath] = true
		}
	}
	var refused []string
	fset := token.NewFileSet()
	exports := map[string]string{}
	for _, p := range packages {
		if p.Export != "" && !ofModule(p) {
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
		report: map[string]bool{}, read: map[string]bool{},
		info: &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	plantedIn := map[string][]plant{}
	for _, p := range plants {
		dir := filepath.Join(module, p.dir)
		plantedIn[dir] = append(plantedIn[dir], p)
	}
	checked := map[string]*types.Package{}
	var read []*pkg
	for _, p := range packages {
		if !ofModule(p) {
			continue
		}
		if p.Error != nil {
			refused = append(refused, fmt.Sprintf("the package %s cannot be loaded: %s", p.ImportPath, strings.TrimSpace(p.Error.Err)))
			continue
		}
		this := &pkg{path: p.ImportPath, command: p.Name == "main"}
		imports := map[string]bool{}
		for _, path := range p.Imports {
			if inModule[path] {
				imports[path] = true
			}
		}
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
			if rel, err := filepath.Rel(root, path); err == nil {
				a.report[rel] = true
			}
			this.files = append(this.files, f)
		}
		for _, planted := range plantedIn[p.Dir] {
			path := filepath.Join(root, planted.name)
			f, err := parser.ParseFile(fset, path, planted.text, parser.ParseComments)
			if err != nil {
				fail(err)
			}
			a.text[path] = []byte(planted.text)
			a.report[planted.name] = true
			this.files = append(this.files, f)
			// A planted file's imports are its package's too.
			for _, spec := range f.Imports {
				if path, err := strconv.Unquote(spec.Path.Value); err == nil && inModule[path] {
					imports[path] = true
				}
			}
		}
		delete(plantedIn, p.Dir)
		for path := range imports {
			this.imports = append(this.imports, path)
		}
		sort.Strings(this.imports)
		if len(this.files) == 0 {
			continue
		}
		var typeErrors []string
		conf := types.Config{
			Importer: importerFunc(func(path string) (*types.Package, error) {
				if pkg := checked[path]; pkg != nil {
					return pkg, nil
				}
				return gc.Import(path)
			}),
			Error: func(err error) {
				if len(typeErrors) < 5 {
					typeErrors = append(typeErrors, err.Error())
				}
			},
		}
		checkedPkg, _ := conf.Check(p.ImportPath, fset, this.files, a.info)
		if len(typeErrors) > 0 {
			refused = append(refused, fmt.Sprintf("the type check of the package %s failed: %s", p.ImportPath, strings.Join(typeErrors, "; ")))
		}
		checked[p.ImportPath] = checkedPkg
		a.read[p.ImportPath] = true
		read = append(read, this)
	}
	for dir, left := range plantedIn {
		for _, planted := range left {
			refused = append(refused, fmt.Sprintf("the planted file %s names the directory %s, which holds no package of the module", planted.name, dir))
		}
	}
	// The redact package is read for its types, never followed into: each of its functions ends a
	// value's path (sanitizer), and what it is handed is the code's own judgement that the value is
	// a URL.
	for _, p := range read {
		for _, f := range p.files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					if fn, ok := a.info.Defs[fd.Name].(*types.Func); ok && !sanitizer(fn) {
						a.bodies[fn] = fd
					}
				}
			}
		}
	}
	return a, read, refused
}

// units are the sets of packages the search follows settings through, each apart: each command
// with the packages it imports, since a setting reaches a print only inside one process, and one
// more of every package no command imports, with the packages those import.
func units(packages []*pkg) (names []string, sets [][]*pkg) {
	byPath := map[string]*pkg{}
	for _, p := range packages {
		byPath[p.path] = p
	}
	closure := func(roots []string) map[string]bool {
		seen := map[string]bool{}
		stack := append([]string(nil), roots...)
		for len(stack) > 0 {
			path := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[path] || byPath[path] == nil {
				continue
			}
			seen[path] = true
			stack = append(stack, byPath[path].imports...)
		}
		return seen
	}
	ordered := func(in map[string]bool) []*pkg {
		var out []*pkg
		for _, p := range packages {
			if in[p.path] {
				out = append(out, p)
			}
		}
		return out
	}
	covered := map[string]bool{}
	for _, p := range packages {
		if !p.command {
			continue
		}
		set := closure([]string{p.path})
		for path := range set {
			covered[path] = true
		}
		names = append(names, p.path)
		sets = append(sets, ordered(set))
	}
	var rest []string
	for _, p := range packages {
		if !covered[p.path] {
			rest = append(rest, p.path)
		}
	}
	if len(rest) > 0 {
		names = append(names, "the packages no command imports")
		sets = append(sets, ordered(closure(rest)))
	}
	return names, sets
}

// reset clears what one unit's run grew, and gives the next unit its files' function literals and
// blocks.
func (a *analysis) reset(files []*ast.File) {
	a.labels, a.ids = nil, map[string]int{}
	a.carried, a.symCarried, a.returns = map[any]set{}, map[any]set{}, map[any]set{}
	a.sites, a.symSites = map[*types.Var]map[token.Pos]*site{}, map[*types.Var]map[token.Pos]*site{}
	a.urls, a.memo = map[int]bool{}, nil
	a.symbolic, a.collect, a.changed = false, false, false
	a.lits, a.blocks, a.impls = nil, nil, map[[2]any]*types.Func{}
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
	}
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "setting-prints:", err)
	os.Exit(1)
}
