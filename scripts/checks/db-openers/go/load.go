//go:build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// openerPackages are the packages whose functions can open or parse a Postgres connection string,
// and pgsetting, the formatter: each is read from its export data, so its functions are known by
// their real signatures. Every other package outside the module is an empty one.
var openerPackages = regexp.MustCompile(`^(database/sql|github\.com/jackc/pgx(/v[0-9]+)?(/pgconn|/pgxpool|/stdlib)?|github\.com/jackc/pgconn|github\.com/lib/pq|github\.com/sjawhar/envoy/pgsetting)$`)

// plantedTag is the build tag every planted shapes' file needs, so that a run which leaves out a
// file only a build tag builds reads none of them, and says so.
const plantedTag = "db_openers_planted"

// module is one build of a module: the files one build context compiles, read from source and
// type-checked together.
type module struct {
	root, dir, path string
	// build names the build context, for the summary.
	build string
	fset  *token.FileSet
	info  *types.Info
	files []*ast.File
	// sources holds each file's bytes, for the text of a call.
	sources map[string][]byte
	// packages holds each package read, module and opener packages alike, by import path.
	packages   map[string]*types.Package
	typeErrors int
}

// source is one non-test Go file of a module as read from disk (or the planted shapes): its
// package's directory, its path, its bytes, the package its clause names and the build tags its
// //go:build line names that no build context sets by itself (not a GOOS, a GOARCH, unix or a
// release).
type source struct {
	dir, path, pkg string
	src            []byte
	tags           []string
}

// loaded is a module read whole: one build for each build context the search needed so that
// every file is in one, and what it read.
type loaded struct {
	builds []*module
	// files counts every file read; constrained counts those the host's own build leaves out
	// (the planted shapes apart).
	files, constrained int
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+"?([^"\s]+)"?`)

// load reads the module in moduleDir (a directory of root): every non-test Go file of it, whatever
// its build constraints, in as many builds as it takes. The first build is the host's own. Each
// file it leaves out then picks the first build context that compiles it (a GOOS and GOARCH `go
// tool dist list` names, the host's first, with a set of the tags its //go:build line names), and
// that context's build reads every file it compiles. A file no context compiles is read in a build
// of its own with the host's files. Each build is type-checked on its own, so that a file and the
// file it stands in for on another system never meet. The planted shapes, when plantDir names a
// package, are more files of it (plantedFiles), which only plantedTag and other tags build.
func load(root, moduleDir, plantDir string) loaded {
	dir := filepath.Join(root, moduleDir)
	goMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		fail(err)
	}
	match := moduleLine.FindSubmatch(goMod)
	if match == nil {
		fail(fmt.Errorf("%s/go.mod names no module", moduleDir))
	}
	modulePath := string(match[1])
	platforms := distList()
	reserved := map[string]bool{"unix": true}
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		reserved[goos], reserved[goarch] = true, true
	}
	var sources []*source
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != dir && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if p != dir {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sources = append(sources, read(filepath.Dir(p), p, src, reserved))
		return nil
	})
	if walkErr != nil {
		fail(walkErr)
	}
	plantedPaths := map[string]bool{}
	if plantDir != "" {
		pkgDir := filepath.Join(dir, plantDir)
		name := majority(sources, pkgDir)
		if name == "" {
			fail(fmt.Errorf("no package of %s is in %s, the planted shapes' package", moduleDir, plantDir))
		}
		for _, f := range plantedFiles {
			p := filepath.Join(root, f.name)
			plantedPaths[p] = true
			sources = append(sources, read(pkgDir, p, []byte(strings.Replace(f.text, "package main", "package "+name, 1)), reserved))
		}
	}

	host := build.Default
	contexts := []build.Context{host}
	names := []string{host.GOOS + "/" + host.GOARCH}
	covered := map[*source]bool{}
	cover := func(ctxt build.Context) {
		for _, s := range sources {
			if compiles(ctxt, s) {
				covered[s] = true
			}
		}
	}
	cover(host)
	result := loaded{}
	for _, s := range sources {
		result.files++
		if !covered[s] && !plantedPaths[s.path] {
			result.constrained++
		}
	}
	var forced []*source
	for _, s := range sources {
		if covered[s] {
			continue
		}
		ctxt, name, ok := contextFor(s, host, platforms)
		if !ok {
			forced = append(forced, s)
			covered[s] = true
			continue
		}
		contexts = append(contexts, ctxt)
		names = append(names, name)
		cover(ctxt)
	}

	exports := map[string]string{}
	for i, ctxt := range contexts {
		var chosen []*source
		for _, s := range sources {
			if compiles(ctxt, s) {
				chosen = append(chosen, s)
			}
		}
		result.builds = append(result.builds, check(root, dir, modulePath, names[i], chosen, exports))
	}
	if len(forced) > 0 {
		var chosen []*source
		for _, s := range sources {
			if compiles(host, s) {
				chosen = append(chosen, s)
			}
		}
		result.builds = append(result.builds, check(root, dir, modulePath, "no build context: read as written", append(chosen, forced...), exports))
	}
	return result
}

// read is the file at p, of the package in dir, with its package and its own build tags.
func read(dir, p string, src []byte, reserved map[string]bool) *source {
	s := &source{dir: dir, path: p, src: src}
	f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		fail(err)
	}
	s.pkg = f.Name.Name
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				continue
			}
			eachTag(expr, func(tag string) {
				if !reserved[tag] && !strings.HasPrefix(tag, "go1.") && !slices.Contains(s.tags, tag) {
					s.tags = append(s.tags, tag)
				}
			})
		}
	}
	sort.Strings(s.tags)
	return s
}

// eachTag calls visit with each tag e names, on every side of each operator.
func eachTag(e constraint.Expr, visit func(string)) {
	switch x := e.(type) {
	case *constraint.TagExpr:
		visit(x.Tag)
	case *constraint.NotExpr:
		eachTag(x.X, visit)
	case *constraint.AndExpr:
		eachTag(x.X, visit)
		eachTag(x.Y, visit)
	case *constraint.OrExpr:
		eachTag(x.X, visit)
		eachTag(x.Y, visit)
	}
}

// majority is the package most files of dir declare (the first by name on a tie), or "".
func majority(sources []*source, dir string) string {
	count := map[string]int{}
	for _, s := range sources {
		if s.dir == dir {
			count[s.pkg]++
		}
	}
	best := ""
	for name, n := range count {
		if best == "" || n > count[best] || (n == count[best] && name < best) {
			best = name
		}
	}
	return best
}

// compiles reports whether a build in ctxt compiles s, by its name and its //go:build line.
func compiles(ctxt build.Context, s *source) bool {
	ctxt.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(s.src)), nil }
	ok, err := ctxt.MatchFile(filepath.Dir(s.path), filepath.Base(s.path))
	return err == nil && ok
}

// contextFor is the first build context that compiles s: each platform, the host's first, with
// each set of s's own tags, fewest first; ok is false when none does.
func contextFor(s *source, host build.Context, platforms []string) (build.Context, string, bool) {
	hostPlatform := host.GOOS + "/" + host.GOARCH
	ordered := append([]string{hostPlatform}, slices.DeleteFunc(slices.Clone(platforms), func(p string) bool { return p == hostPlatform })...)
	tags := s.tags
	if len(tags) > 10 {
		tags = tags[:10]
	}
	var sets [][]string
	for mask := range 1 << len(tags) {
		var set []string
		for i, tag := range tags {
			if mask&(1<<i) != 0 {
				set = append(set, tag)
			}
		}
		sets = append(sets, set)
	}
	sort.SliceStable(sets, func(i, j int) bool { return len(sets[i]) < len(sets[j]) })
	for _, p := range ordered {
		goos, goarch, _ := strings.Cut(p, "/")
		for _, set := range sets {
			ctxt := host
			ctxt.GOOS, ctxt.GOARCH = goos, goarch
			ctxt.CgoEnabled = false
			ctxt.BuildTags = set
			if compiles(ctxt, s) {
				name := p
				if len(set) > 0 {
					name += " with " + strings.Join(set, ",")
				}
				return ctxt, name, true
			}
		}
	}
	return build.Context{}, "", false
}

// distList is every GOOS/GOARCH pair `go tool dist list` names.
func distList() []string {
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		fail(fmt.Errorf("go tool dist list: %w", err))
	}
	return strings.Fields(string(out))
}

// check is the build of the module in dir (at modulePath) that compiles chosen: each package type-
// checked once its module dependencies are. A directory's package is the one most of its chosen
// files declare; a file that declares another (a program only a tag builds, say) is a package of
// its own, so that it is read too. exports caches the export data of the opener packages.
func check(root, dir, modulePath, name string, chosen []*source, exports map[string]string) *module {
	m := &module{
		root: root, dir: dir, path: modulePath, build: name, fset: token.NewFileSet(),
		sources: map[string][]byte{}, packages: map[string]*types.Package{},
		info: &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	byPath := map[string][]*ast.File{}
	for _, s := range chosen {
		f, err := parser.ParseFile(m.fset, s.path, s.src, parser.ParseComments)
		if err != nil {
			fail(err)
		}
		m.sources[s.path] = s.src
		importPath := m.importPath(s.dir)
		if s.pkg != majority(chosen, s.dir) {
			importPath += " (package " + s.pkg + ")"
		}
		byPath[importPath] = append(byPath[importPath], f)
	}
	imports := map[string]map[string]bool{}
	needed := map[string]bool{}
	for importPath, files := range byPath {
		imports[importPath] = map[string]bool{}
		for _, f := range files {
			for _, spec := range f.Imports {
				p := strings.Trim(spec.Path.Value, "\"`")
				imports[importPath][p] = true
				if _, inModule := byPath[p]; !inModule && openerPackages.MatchString(p) {
					needed[p] = true
				}
			}
		}
	}
	gc := m.exportImporter(needed, exports)
	var order []string
	seen := map[string]bool{}
	var visit func(string)
	visit = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		var deps []string
		for dep := range imports[p] {
			if _, inModule := byPath[dep]; inModule {
				deps = append(deps, dep)
			}
		}
		sort.Strings(deps)
		for _, dep := range deps {
			visit(dep)
		}
		order = append(order, p)
	}
	var all []string
	for p := range byPath {
		all = append(all, p)
	}
	sort.Strings(all)
	for _, p := range all {
		visit(p)
	}
	for _, p := range order {
		conf := types.Config{
			Importer: importerFunc(func(importPath string) (*types.Package, error) {
				if pkg := m.packages[importPath]; pkg != nil {
					return pkg, nil
				}
				if needed[importPath] {
					pkg, err := gc.Import(importPath)
					if err != nil {
						return nil, err
					}
					m.packages[importPath] = pkg
					return pkg, nil
				}
				// An empty package: what the code names in it does not resolve, which can make no
				// call an opener.
				pkg := types.NewPackage(importPath, packageName(importPath))
				pkg.MarkComplete()
				m.packages[importPath] = pkg
				return pkg, nil
			}),
			FakeImportC: true,
			Error:       func(error) { m.typeErrors++ },
		}
		pkg, _ := conf.Check(p, m.fset, byPath[p], m.info)
		m.packages[p] = pkg
		m.files = append(m.files, byPath[p]...)
	}
	return m
}

// importPath is the import path of the module's package in dir.
func (m *module) importPath(dir string) string {
	rel, err := filepath.Rel(m.dir, dir)
	if err != nil || rel == "." {
		return m.path
	}
	return m.path + "/" + filepath.ToSlash(rel)
}

// exportImporter is an importer of the packages needed from the export data `go list -export`
// writes for them, run in the module's directory with the environment this program was given;
// exports holds what earlier builds of the module listed.
func (m *module) exportImporter(needed map[string]bool, exports map[string]string) types.Importer {
	var paths []string
	for p := range needed {
		if _, listed := exports[p]; !listed {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		sort.Strings(paths)
		list := exec.Command("go", append([]string{"list", "-export", "-json=ImportPath,Export"}, paths...)...)
		list.Dir = m.dir
		list.Stderr = os.Stderr
		out, err := list.Output()
		if err != nil {
			fail(fmt.Errorf("go list -export in %s: %w", m.dir, err))
		}
		decoder := json.NewDecoder(bytes.NewReader(out))
		for decoder.More() {
			var p struct{ ImportPath, Export string }
			if err := decoder.Decode(&p); err != nil {
				fail(err)
			}
			exports[p.ImportPath] = p.Export
		}
	}
	return importer.ForCompiler(m.fset, "gc", func(importPath string) (io.ReadCloser, error) {
		file, ok := exports[importPath]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", importPath)
		}
		return os.Open(file)
	})
}

// packageName is the name a package at importPath most likely declares: its last element, without
// a major version, a "go-" prefix or a ".vN" suffix.
func packageName(importPath string) string {
	name := path.Base(importPath)
	if len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = path.Base(path.Dir(importPath))
	}
	name = strings.TrimPrefix(name, "go-")
	if i := strings.Index(name, ".v"); i > 0 {
		name = name[:i]
	}
	return strings.NewReplacer("-", "_", ".", "_").Replace(name)
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
