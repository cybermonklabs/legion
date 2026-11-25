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
	"slices"
	"sort"
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
// packages is parsed and type-checked, as each build CI makes reads it: each module is listed
// once for each build CI makes of it (ciBuilds: each of ciTargets with each tag list its CI vet
// passes), and each package is type-checked for every set of files one of those builds compiles it
// from, so a file built for one target or tag list only is read like any other. A file no such
// build compiles, found by walking the module's directories (sourceFiles), is read by its syntax
// alone (untargeted). A file that cannot be read is UNKNOWN.
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
		g.goModuleCensus(&c, dir, mod, files, rule, printers)
	}
	c.dedupe()
	return c
}

// goListed is one package as go list -json answers it.
type goListed struct {
	ImportPath, Dir, Export string
	GoFiles, CgoFiles       []string
	Deps                    []string // every package it imports, directly or not
	Standard, DepOnly       bool
	Error                   *struct{ Err string }
}

// sourceFiles are the production Go files of the module at modDir, found on disk as go's ./...
// pattern walks it (cmd/go's matchPackages): every .go file that is not a test's, outside a
// testdata directory, a directory or file whose name starts with "." or "_", a directory below a
// vendor directory (a vendor directory's own files go reads as a package), and a directory that
// holds a go.mod of its own (another module, which goModules lists). go list names no package for
// a directory whose every file the build excludes, and no error, so the files go list names are
// not all a module's: this walk is what the census holds every file it read against. go.mod's
// ignore directive, which no module here uses, is not read: a directory it names is walked.
func sourceFiles(modDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(modDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path == modDir {
				return nil
			}
			parent := filepath.Dir(path)
			if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || (parent != modDir && filepath.Base(parent) == "vendor") || fileExists(filepath.Join(path, "go.mod")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// goList lists patterns in the module at modDir for build b, with -export -deps where export is
// set, so each dependency's export data is built for that target.
func goList(modDir string, b ciBuild, export bool, patterns ...string) ([]goListed, error) {
	args := []string{"list", "-e", "-json"}
	if export {
		args = append(args, "-export", "-deps")
	}
	if b.Tags != "" {
		args = append(args, "-tags="+b.Tags)
	}
	list := exec.Command("go", append(args, patterns...)...)
	list.Dir = modDir
	list.Env = append(os.Environ(), b.env()...)
	var stderr bytes.Buffer
	list.Stderr = &stderr
	// -e reports a package that does not build in its Error and goes on; a broken file is then
	// that package's UNKNOWN, and every other package is still read.
	out, err := list.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("%v: %s", err, lastLines(stderr.String(), 5))
	}
	var pkgs []goListed
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p goListed
		if err := dec.Decode(&p); err != nil {
			return pkgs, fmt.Errorf("go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// goModuleCensus is goCensus over the module at mod: listed for each target and tag list,
// type-checked once for each set of files a package is compiled from, and its files no build
// compiles (sourceFiles less the ones a build compiled) read by syntax. It reports, for each
// target and tag list, how many packages and files the build compiles and how many packages it
// type-checked that no build before it had.
func (g *gate) goModuleCensus(c *census, dir, mod string, files map[string]map[int]bool, rule, printers map[string]bool) {
	modDir := filepath.Join(dir, mod)
	builds, err := g.ciBuilds(dir, mod)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("Go: the build tags of %s cannot be read: %v", mod, err))
		return
	}
	checked := map[string]bool{} // a package's import path and the files one build compiles it from
	built := map[string]bool{}   // every file some build compiles
	for _, b := range builds.all() {
		listed, err := goList(modDir, b, false, "./...")
		if err != nil {
			c.Unknown = append(c.Unknown, fmt.Sprintf("Go: go list in %s for %s, tags %q: %v", mod, b.ciTarget, b.Tags, err))
			continue
		}
		var need []string
		packages, compiled := 0, 0
		for _, p := range listed {
			if p.Standard || p.DepOnly {
				continue
			}
			packages++
			compiled += len(p.GoFiles)
			for _, name := range p.GoFiles {
				built[filepath.Join(p.Dir, name)] = true
			}
			if len(p.CgoFiles) > 0 {
				c.Unknown = append(c.Unknown, p.ImportPath+": "+cgoUnread(p.CgoFiles))
			}
			key := p.ImportPath + "\x00" + strings.Join(p.GoFiles, "\x00")
			if len(p.GoFiles) == 0 || checked[key] {
				continue
			}
			checked[key] = true
			need = append(need, p.ImportPath)
		}
		c.Targets = append(c.Targets, fmt.Sprintf("Go module %s for %s, tags %q: %d packages, %d production files compiled, %d packages type-checked on a file set no build before it compiles", mod, b.ciTarget, b.Tags, packages, compiled, len(need)))
		if len(need) == 0 {
			continue
		}
		deps, err := goList(modDir, b, true, need...)
		if err != nil {
			c.Unknown = append(c.Unknown, fmt.Sprintf("Go: go list -export in %s for %s, tags %q: %v", mod, b.ciTarget, b.Tags, err))
			continue
		}
		build := newGoBuild(deps)
		for _, p := range deps {
			g.recordImports(p.ImportPath, p.Deps)
		}
		wanted := set(need)
		for _, p := range deps {
			if wanted[p.ImportPath] && !p.DepOnly {
				g.goPackage(c, dir, p.ImportPath, build, p.Error != nil, files, rule, printers)
			}
		}
	}
	present, err := sourceFiles(modDir)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("Go: the files of %s cannot be walked: %v", mod, err))
		return
	}
	// Control of the walk: every file a build compiled is one it found, so a walk that names files
	// otherwise than go list does, or finds none, is UNKNOWN rather than a module of untargeted files.
	walked := set(present)
	for file := range built {
		if !walked[file] {
			c.Unknown = append(c.Unknown, fmt.Sprintf("Go: a build of %s compiles %s, which the walk of its files did not find", mod, file))
		}
	}
	var untargeted []string
	for _, file := range present {
		if !built[file] {
			untargeted = append(untargeted, file)
		}
	}
	sort.Strings(untargeted)
	for _, file := range untargeted {
		g.untargeted(c, dir, file)
	}
}

// untargetedImports are the packages a file no CI build compiles may not import: those that make
// or read an HTTP answer, whose use the census reads only type-checked.
var untargetedImports = []string{"net/http", "net/http/httputil", "github.com/sjawhar/envoy/internal/redact", "github.com/sjawhar/envoy/oauth2call"}

// untargeted reads file, a production file no build of ciTargets and the CI tag lists compiles
// (a Windows-only file, a race-only one, one in a directory no such build lists), by its syntax
// alone, since no one type-check can read it: it is listed as such where it imports none of
// untargetedImports and selects no Body or Status, and is UNKNOWN otherwise.
func (g *gate) untargeted(c *census, dir, file string) {
	rel, _ := filepath.Rel(dir, file)
	rel = filepath.ToSlash(rel)
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: cannot be parsed: %v", rel, err))
		return
	}
	var why []string
	for _, spec := range f.Imports {
		if path := strings.Trim(spec.Path.Value, `"`); slices.Contains(untargetedImports, path) {
			why = append(why, "imports "+path)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Body" || sel.Sel.Name == "Status") {
			why = append(why, "selects ."+sel.Sel.Name)
		}
		return true
	})
	if len(why) > 0 {
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: no CI build compiles it, and it %s, which the census reads only type-checked", rel, strings.Join(slices.Compact(why), ", ")))
		return
	}
	c.Untargeted = append(c.Untargeted, rel)
}

// goSource is a listed package's directory, and its files and cgo files as one build compiles it,
// with the export data of every package that build compiles (go list -export -deps): what
// type-checks it from source.
type goSource struct {
	dir     string
	files   []string
	cgo     []string
	exports map[string]string
}

// goBuild is one build's listing: each package's source, by import path (newGoBuild).
type goBuild map[string]goSource

// newGoBuild is the build deps, one go list -export -deps, lists: each package it names, with the
// export data of them all.
func newGoBuild(deps []goListed) goBuild {
	exports := map[string]string{}
	build := goBuild{}
	for _, p := range deps {
		exports[p.ImportPath] = p.Export
		build[p.ImportPath] = goSource{dir: p.Dir, files: p.GoFiles, cgo: p.CgoFiles, exports: exports}
	}
	return build
}

// parse parses the package's files into fset, or answers the file that cannot be parsed and why.
func (s goSource) parse(fset *token.FileSet) ([]*ast.File, string, error) {
	var parsed []*ast.File
	for _, name := range s.files {
		file := filepath.Join(s.dir, name)
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, file, err
		}
		parsed = append(parsed, f)
	}
	return parsed, "", nil
}

// importer reads each package the package imports from its build's export data.
func (s goSource) importer(fset *token.FileSet) types.Importer {
	return importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		if e := s.exports[p]; e != "" {
			return os.Open(e)
		}
		return nil, fmt.Errorf("no export data for %s", p)
	})
}

// cgoUnread says that the census does not read files, a package's cgo files.
func cgoUnread(files []string) string {
	return "cgo files (" + strings.Join(files, ", ") + "), which the census does not read"
}

// goPackage type-checks one package from source, as build lists it, lists its rule references on
// changed lines (the call census), and runs the box, holder, reflection and answer censuses over
// every file of it.
func (g *gate) goPackage(c *census, dir, path string, build goBuild, broken bool, files map[string]map[int]bool, rule, printers map[string]bool) {
	src := build[path]
	fset := token.NewFileSet()
	parsed, failed, err := src.parse(fset)
	if err != nil {
		rel, _ := filepath.Rel(dir, failed)
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: cannot be parsed: %v", filepath.ToSlash(rel), err))
		return
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
	conf := types.Config{Importer: src.importer(fset)}
	checked, err := conf.Check(path, fset, parsed, info)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: cannot be resolved: %v", path, err))
		return
	}
	g.structural(c, dir, path, build, fset, parsed, info)
	response, err := httpResponse(checked, conf.Importer)
	if err != nil {
		c.Unknown = append(c.Unknown, fmt.Sprintf("%s: %v", path, err))
		return
	}
	g.answerCensus(c, dir, path, fset, parsed, info, checked, response)
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
