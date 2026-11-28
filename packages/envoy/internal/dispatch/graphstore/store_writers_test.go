package graphstore

import (
	"bytes"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// moduleRoot is the packages/envoy module, read from this package's own directory.
const moduleRoot = "../../.."

// graphstoreImportPath is the path a file outside this package names the store by.
const graphstoreImportPath = "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// graphstoreDirectory is this package's own directory under moduleRoot, where its writers are
// named without a package.
const graphstoreDirectory = "internal/dispatch/graphstore"

// graphseedtestDirectory is the one package besides the store's own whose statements write its
// tables: graphseedtest, which seeds for other packages' tests the rows the store's writers
// refuse to write, and which no deployment builds. Any other package under the store's directory
// is held to the rule like every package outside it.
const graphseedtestDirectory = graphstoreDirectory + "/graphseedtest"

// storeWriteFunctions are the calls a write of graph_nodes and graph_tuples from outside this
// package goes through (graphstoretest.WriteFunctions), the same ones docs' register of the
// tree's passes scans for (storeWriterCall), and the session writers beside them, held in
// graphstoretest so api's scan of the projections' writers reads the same list. A write that
// holds a statement of its own instead is found by rawStoreWrite, and none outside this package
// may hold one.
var storeWriteFunctions = graphstoretest.WriteFunctions()

// rawStoreWrite matches a statement that writes graph_nodes or graph_tuples itself rather than
// through one of storeWriteFunctions: an insert, an update or a delete, in any case, over any
// whitespace, and under the schema's name as well as bare.
var rawStoreWrite = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+(?:public\s*\.\s*)?graph_(?:nodes|tuples)\b`)

// storeWriteCallSites is every function outside the tests that calls one of storeWriteFunctions,
// each beside the test that drives its lock steps, as the register of the store's writers holds
// them (lockstep.StoreWriteCallSites). The register is held there rather than here so that
// graphcensus's TestTheSizingCountsEveryTupleTheWritersAppend, which drives every site it lists,
// reads the one list this test holds to the module's source.
var storeWriteCallSites = lockstep.StoreWriteCallSites()

// TestEveryStoreWriteCallSiteIsListed reads the module's source, every Go file but the tests,
// and finds each function that names one of storeWriteFunctions, called or passed on as a value,
// and each function outside this package whose string literals hold a statement writing
// graph_nodes or graph_tuples (rawStoreWrite). It fails on a site storeWriteCallSites does not
// list, naming the site and the call, so a new writer of the store cannot appear without its lock
// steps being proven, and on a listed site the source no longer holds, so the list never names a
// writer that is gone. It fails on every statement outside this package that writes the store,
// naming the site and the statement: this package is the one place that writes those tables, so
// such a statement moves behind a writer here. It parses the source rather than matching its
// text, so a call written over two lines, a comment that names a writer or spells a statement,
// and a method of another type that shares a writer's name are each read for what they are.
//
// It then reads the driver storeWriteCallSites names beside each site, and fails on a site that
// names none, and on one whose driver names a test no _test.go file of its package declares, read
// from those files as `go test` builds them with no build tag (requireALockStepDriver): either is
// a listed site whose lock steps no test of c-lock-order's run drives.
func TestEveryStoreWriteCallSiteIsListed(t *testing.T) {
	found, raw := map[string][]string{}, map[string][]string{}
	parsed := 0
	walkModuleSources(t, func(source []byte) bool {
		// A file that names the store names it by its import path or is the store's own
		// package, and both spell the word; a file that writes the store's tables itself spells
		// one of their names, in whatever case its statement is written in. The rest of the
		// module is never parsed.
		lowered := bytes.ToLower(source)
		return bytes.Contains(source, []byte("graphstore")) ||
			bytes.Contains(lowered, []byte("graph_nodes")) || bytes.Contains(lowered, []byte("graph_tuples"))
	}, func(relative string, source []byte) {
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", relative, err)
		}
		parsed++
		for site, calls := range storeWriteSitesIn(relative, files, file) {
			found[site] = append(found[site], calls...)
		}
		for site, statements := range rawStoreWritesIn(relative, files, file) {
			raw[site] = append(raw[site], statements...)
		}
	})
	if parsed == 0 || len(found) == 0 {
		t.Fatalf("read %d files naming graphstore or its tables and found %d store writers: the walk read nothing worth checking", parsed, len(found))
	}
	t.Logf("read %d files naming graphstore or its tables under %s and found %d store writer sites", parsed, moduleRoot, len(found))

	sites := make([]string, 0, len(found))
	for site := range found {
		sites = append(sites, site)
	}
	slices.Sort(sites)
	for _, site := range sites {
		t.Logf("store writer %s (%s), its lock steps driven by %s", site, strings.Join(found[site], ", "), storeWriteCallSites[site])
		if _, listed := storeWriteCallSites[site]; !listed {
			t.Errorf("%s writes the store (%s) and is not in storeWriteCallSites: a new writer of graph_nodes and graph_tuples takes the lock order written on api/inbox_read.go's markInboxReadThreads, so a test of its own package drives its steps and then it is listed here beside that test",
				site, strings.Join(found[site], ", "))
		}
	}
	tests := map[string]map[string]bool{}
	for _, site := range slices.Sorted(maps.Keys(storeWriteCallSites)) {
		if _, ok := found[site]; !ok {
			t.Errorf("storeWriteCallSites lists %s, and no file of the module holds a call of a store writer or a statement writing the store there any more: take it off the list, since the proofs of the writers rest on it naming the ones there are", site)
		}
		requireALockStepDriver(t, site, storeWriteCallSites[site], tests)
	}
	for _, site := range slices.Sorted(maps.Keys(raw)) {
		t.Errorf("%s writes the store with a statement of its own (%s): graphstore is the one place that writes graph_nodes and graph_tuples, so the statement moves behind one of its writers (storeWriteFunctions) and the site calls that writer",
			site, strings.Join(raw[site], ", "))
	}
}

// lockStepDriver is a driver as storeWriteCallSites names one: a package's directory, a '#', and
// the name of a test.
var lockStepDriver = regexp.MustCompile(`^([a-z0-9][a-z0-9_./-]*)#(Test\w*)$`)

// requireALockStepDriver fails unless driver, the test storeWriteCallSites names beside site, is a
// test the package it names declares in a _test.go file `go test` builds with no build tag, which
// is how c-lock-order's run builds it. A site with no driver, or with one that names a test no such
// file declares, is a site whose lock steps the run drives no test of. tests holds each package's
// tests by the directory a driver names, read once.
func requireALockStepDriver(t *testing.T, site, driver string, tests map[string]map[string]bool) {
	t.Helper()
	match := lockStepDriver.FindStringSubmatch(driver)
	if match == nil {
		t.Errorf("storeWriteCallSites names %q beside %s, which is no test that drives its lock steps: name one as \"<package dir>#<TestName>\", the package's directory named as a site's file is (from internal/dispatch where it lives under it), and a test of that package that runs %s and holds each of its steps in another transaction to read where it waits",
			driver, site, site)
		return
	}
	directory, name := match[1], match[2]
	declared, read := tests[directory]
	if !read {
		declared = testsDeclaredIn(t, directory)
		tests[directory] = declared
	}
	if !declared[name] {
		// The directory as the module names it, which is how a reader finds the package.
		shown, err := filepath.Rel(moduleRoot, driverDirectory(directory))
		if err != nil {
			shown = driverDirectory(directory)
		}
		t.Errorf("storeWriteCallSites names %s as the test that drives the lock steps of %s, and no _test.go file of %s that `go test` builds declares func %s(*testing.T): c-lock-order's run drives no test of %s until one does",
			driver, site, filepath.ToSlash(shown), name, site)
	}
}

// testsDeclaredIn is every test the package in directory declares, by name, read from the
// _test.go files `go test` builds for it with no build tag (build.Default), so a test that only a
// tagged build compiles is none. A directory that is not there declares none.
func testsDeclaredIn(t *testing.T, directory string) map[string]bool {
	t.Helper()
	path := driverDirectory(directory)
	declared := map[string]bool{}
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return declared
	}
	if err != nil {
		t.Fatalf("read the test files of %s: %v", path, err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		built, err := build.Default.MatchFile(path, entry.Name())
		if err != nil {
			t.Fatalf("read the build constraints of %s: %v", filepath.Join(path, entry.Name()), err)
		}
		if !built {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(path, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(path, entry.Name()), err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && isTestFunction(function) {
				declared[function.Name.Name] = true
			}
		}
	}
	return declared
}

// isTestFunction is whether function is one `go test` runs as a test: a function, not a method,
// whose one parameter is a *testing.T and which answers nothing.
func isTestFunction(function *ast.FuncDecl) bool {
	if function.Recv != nil || function.Type.Results != nil {
		return false
	}
	parameters := function.Type.Params.List
	if len(parameters) != 1 || len(parameters[0].Names) > 1 {
		return false
	}
	pointer, ok := parameters[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	named, ok := pointer.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	qualifier, ok := named.X.(*ast.Ident)
	return ok && qualifier.Name == "testing" && named.Sel.Name == "T"
}

// driverDirectory is the directory a driver names, read as siteKey writes a site's file: under
// internal/dispatch where such a directory is, and from the module otherwise.
func driverDirectory(directory string) string {
	under := filepath.Join(moduleRoot, "internal", "dispatch", filepath.FromSlash(directory))
	if info, err := os.Stat(under); err == nil && info.IsDir() {
		return under
	}
	return filepath.Join(moduleRoot, filepath.FromSlash(directory))
}

// skippedDirectory is a directory the walk does not enter: one Go itself ignores (testdata, a
// name starting with a dot or an underscore), a vendored or installed tree, and another module.
func skippedDirectory(path, name string) bool {
	if name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}

// storeWriteSitesIn is every site one file names a store writer at, each with the calls it makes
// as "graphstore.<writer> at <file>:<line>". In the store's own package the writers are named
// bare; anywhere else through the name the file imports graphstore under.
func storeWriteSitesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	bare := filepath.ToSlash(filepath.Dir(relative)) == graphstoreDirectory && file.Name.Name == "graphstore"
	qualifier := ""
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != graphstoreImportPath {
			continue
		}
		switch {
		case spec.Name == nil:
			qualifier = "graphstore"
		case spec.Name.Name == ".":
			bare = true
		case spec.Name.Name != "_":
			qualifier = spec.Name.Name
		}
	}
	if !bare && qualifier == "" {
		return nil
	}

	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		var body ast.Node = declaration
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
			if decl.Body == nil {
				continue
			}
			body = decl.Body
		}
		record := func(name *ast.Ident) {
			line := files.Position(name.Pos()).Line
			sites[site] = append(sites[site], "graphstore."+name.Name+" at "+relative+":"+strconv.Itoa(line))
		}
		var visit func(node ast.Node) bool
		visit = func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); ok && qualifier != "" && x.Name == qualifier &&
					slices.Contains(storeWriteFunctions, node.Sel.Name) {
					record(node.Sel)
					return false
				}
				// The name after a dot is a field or a method of something else, never the
				// store's own function, so only the expression before it is read on.
				ast.Inspect(node.X, visit)
				return false
			case *ast.Ident:
				// Bare, a writer is named only in the store's own package or in a file that
				// dot-imports it.
				if bare && slices.Contains(storeWriteFunctions, node.Name) {
					record(node)
				}
			}
			return true
		}
		ast.Inspect(body, visit)
	}
	return sites
}

// rawStoreWritesIn is every site one file writes the store at with a statement of its own, each
// with the statements it holds as "<statement> at <file>:<line>", read from the file's string
// literals alone, so a statement a comment spells is none. The store's own package writes its
// tables that way by design, through the writers storeWriteFunctions names, and graphseedtest
// seeds what those writers refuse (graphseedtestDirectory), so the files of those two packages
// alone are not read here.
func rawStoreWritesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	if directory := filepath.ToSlash(filepath.Dir(relative)); directory == graphstoreDirectory || directory == graphseedtestDirectory {
		return nil
	}
	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, match := range rawStoreWrite.FindAllStringIndex(text, -1) {
				// A raw string holds its lines as the source does, so a statement inside one is
				// on the literal's line plus the line breaks before it.
				line := files.Position(literal.Pos()).Line
				if strings.HasPrefix(literal.Value, "`") {
					line += strings.Count(text[:match[0]], "\n")
				}
				statement := strings.ToLower(strings.Join(strings.Fields(text[match[0]:match[1]]), " "))
				sites[site] = append(sites[site], statement+" at "+relative+":"+strconv.Itoa(line))
			}
			return true
		})
	}
	return sites
}

// siteKey names a site as docs' register of the tree's passes names one (passKey): the file,
// from internal/dispatch where it lives under it and from the module otherwise, and the function.
func siteKey(relative, function string) string {
	return strings.TrimPrefix(relative, "internal/dispatch/") + "#" + function
}
