package refs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoProjectsNamed finds the table that held the rename pairs, as a word in any letter case.
var repoProjectsNamed = regexp.MustCompile(`(?i)\brepo_projects\b`)

// repoProjectsReadAllowed is each function of the packages below whose SQL may name repo_projects:
// api's listing of the rows as the last release left them, which names no rename and which no
// reader of a repository name reads. store.ReadRepoRenames, the boot's one read of the pairs, is
// in the store package, which this check does not read.
var repoProjectsReadAllowed = map[string]bool{"api/repo_projects.go:listRepoProjects": true}

// sqlNamingRepoProjects is each string literal of the Go source src, read as file, that names
// repo_projects, as "file:line in function", outside the allowed functions. A comment is no
// literal, so the doc comments that name the table are not counted.
func sqlNamingRepoProjects(t *testing.T, file string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var found []string
	for _, decl := range parsed.Decls {
		name := "package level"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			name = fn.Name.Name
		}
		if repoProjectsReadAllowed[file+":"+name] {
			continue
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				text = literal.Value
			}
			if repoProjectsNamed.MatchString(text) {
				found = append(found, file+":"+strconv.Itoa(fset.Position(literal.Pos()).Line)+" in "+name)
			}
			return true
		})
	}
	return found
}

// TestNoStatementOfTheDeliveryOrAPIPackagesNamesRepoProjects holds that no SQL of the readers of a
// repository name reads the rename pairs from repo_projects: each takes the pairs the boot read
// (Renames) and writes them into its statement (CanonicalRepoSQL, RenamesSQL, NamesSQL), because a
// later change drops the table, and a statement that named a dropped table would fail. It reads
// every non-test Go file of api, delivery, refs and cireconcile, and fails on each string literal
// that names the table, outside api's listing of the rows as the last release left them. Its
// controls are a planted statement, which it must find, and a comment naming the table, which it
// must not count.
func TestNoStatementOfTheDeliveryOrAPIPackagesNamesRepoProjects(t *testing.T) {
	planted := sqlNamingRepoProjects(t, "delivery/planted.go", []byte("package delivery\n\n"+
		"// repo_projects in a comment is no statement.\n"+
		"const planted = `select renamed_to from repo_projects`\n"))
	if len(planted) != 1 || planted[0] != "delivery/planted.go:4 in package level" {
		t.Fatalf("the planted statement is found as %v, want [delivery/planted.go:4 in package level]", planted)
	}

	var found []string
	read := 0
	for _, pkg := range []string{"api", "delivery", "refs", "cireconcile"} {
		files, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatalf("list %s: %v", pkg, err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			read++
			found = append(found, sqlNamingRepoProjects(t, pkg+"/"+filepath.Base(path), src)...)
		}
	}
	if read < 100 {
		t.Fatalf("read %d source files of api, delivery, refs and cireconcile, want the packages whole (at least 100): the check cannot see what it did not read", read)
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Fatalf("%d string literals name repo_projects, which the boot reads once (store.ReadRepoRenames) and a later change drops; take the pairs the boot read (refs.Renames) instead:\n%s",
			len(found), strings.Join(found, "\n"))
	}
}
