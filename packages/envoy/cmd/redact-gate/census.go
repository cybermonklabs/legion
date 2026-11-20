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
	"strings"
)

// goModules are the Go modules the repository builds: each module go.work at dir uses, and every
// module nested in one of them (outside .git, node_modules, testdata and vendor directories), such
// as packages/envoy/infra. A go.mod outside them, a research prototype's, is no module of the build.
func goModules(dir string) ([]string, error) {
	roots, err := workRoots(dir)
	if err != nil {
		return nil, err
	}
	var mods []string
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(dir, root), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "testdata", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == "go.mod" {
				rel, _ := filepath.Rel(dir, filepath.Dir(path))
				mods = append(mods, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return mods, nil
}

// workRoots are the modules go.work at dir uses, relative to dir.
func workRoots(dir string) ([]string, error) {
	work, err := os.ReadFile(filepath.Join(dir, "go.work"))
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, line := range strings.Split(string(work), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "use"))
		if strings.HasPrefix(line, "./") {
			roots = append(roots, strings.TrimPrefix(strings.Trim(line, "()"), "./"))
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("go.work uses no module")
	}
	return roots, nil
}

// moduleWork is the GOWORK module mod of dir loads with, as its own build loads it: the
// repository's go.work for a module it uses (roots); a nested module's own go.work where it has
// one (packages/envoy/infra's uses itself and the envoy module beside it, whose code it builds
// against); and "off", a module of its own, otherwise.
func moduleWork(dir, mod string, roots []string) string {
	if set(roots)[mod] {
		return filepath.Join(dir, "go.work")
	}
	if own := filepath.Join(dir, mod, "go.work"); fileExists(own) {
		return own
	}
	return "off"
}

// fileExists reports whether path names a file that can be read.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// goCensus is the Go half of the call census over dir, the snapshot: every reference the change
// adds or changes whose resolved callee is the rule or a printer built on it (gate.rule,
// gate.printers), found with go/types, so a receiver call, a method value and a call through an
// import or a type alias resolve as a plain call does. Every production file of each module's
// packages is parsed and type-checked; one that cannot be is UNKNOWN.
func (g *gate) goCensus(dir string) census {
	c := census{}
	files, err := g.changed(dir, func(n string) bool {
		return strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")
	})
	if err != nil {
		c.Unknown = append(c.Unknown, "Go: "+err.Error())
		return c
	}
	rule, printers := set(g.inv.Gate.Rule), set(g.inv.Gate.Printers)
	mods, err := goModules(dir)
	if err != nil || len(mods) == 0 {
		c.Unknown = append(c.Unknown, fmt.Sprintf("Go: the tree's modules cannot be listed: %v", err))
		return c
	}
	for _, mod := range mods {
		modDir := filepath.Join(dir, mod)
		list := exec.Command("go", "list", "-e", "-export", "-json", "-deps", "./...")
		list.Dir = modDir
		var stderr bytes.Buffer
		list.Stderr = &stderr
		// -e reports a package that does not build in its Error and goes on; a broken file is
		// then that package's UNKNOWN, and every other package is still read.
		out, err := list.Output()
		if err != nil && len(out) == 0 {
			c.Unknown = append(c.Unknown, fmt.Sprintf("Go: go list in %s: %v: %s", mod, err, lastLines(stderr.String(), 5)))
			continue
		}
		type pkg struct {
			ImportPath, Dir, Export string
			GoFiles                 []string
			Standard, DepOnly       bool
			Error                   *struct{ Err string }
		}
		exports := map[string]string{}
		var mine []pkg
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p pkg
			if err := dec.Decode(&p); err != nil {
				c.Unknown = append(c.Unknown, "Go: go list output: "+err.Error())
				break
			}
			exports[p.ImportPath] = p.Export
			if !p.DepOnly && !p.Standard {
				mine = append(mine, p)
			}
		}
		for _, p := range mine {
			g.goPackage(&c, dir, p.ImportPath, p.Dir, p.GoFiles, p.Error != nil, exports, files, rule, printers)
		}
	}
	return c
}

// goPackage type-checks one package from source, lists its rule references on changed lines
// (the call census), and runs the box, holder and reflection censuses over every file of it.
func (g *gate) goPackage(c *census, dir, path, pkgDir string, goFiles []string, broken bool, exports map[string]string, files map[string]map[int]bool, rule, printers map[string]bool) {
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range goFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			rel, _ := filepath.Rel(dir, filepath.Join(pkgDir, name))
			c.Unknown = append(c.Unknown, fmt.Sprintf("%s: cannot be parsed: %v", filepath.ToSlash(rel), err))
			return
		}
		parsed = append(parsed, f)
	}
	if broken || len(parsed) == 0 {
		if broken {
			c.Unknown = append(c.Unknown, fmt.Sprintf("%s: the package cannot be loaded", path))
		}
		return
	}
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Defs:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
		Instances:  map[*ast.Ident]types.Instance{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		if e := exports[p]; e != "" {
			return os.Open(e)
		}
		return nil, fmt.Errorf("no export data for %s", p)
	})}
	checked, err := conf.Check(path, fset, parsed, info)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: cannot be resolved: %v", path, err))
		return
	}
	g.structural(c, dir, path, fset, parsed, info, checked)
	if entry := g.inv.Gate.BoundaryFields.Entry; len(entry) > 0 {
		if entryPkg, _ := splitTypeName(entry[0]); entryPkg == path {
			g.fieldCensus(c, dir, path, fset, parsed, info)
		}
	}
	for _, f := range parsed {
		rel, _ := filepath.Rel(dir, fset.Position(f.Pos()).Filename)
		rel = filepath.ToSlash(rel)
		lines := files[rel]
		if len(lines) == 0 || g.exemptFile(rel) {
			continue
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			function := goFuncName(path, fd)
			ast.Inspect(fd, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				fn, ok := info.Uses[id].(*types.Func)
				if !ok {
					return true
				}
				full := fn.Origin().FullName()
				if p := fset.Position(id.Pos()); (rule[full] || printers[full]) && lines[p.Line] {
					c.Calls = append(c.Calls, entry{File: rel, Line: p.Line, Offset: p.Offset, Callee: full, Function: function})
				}
				return true
			})
		}
	}
}
