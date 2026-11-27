package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// namedIn is one place a declaration of dir names an identifier: the file, the line, and the
// function declaration's name, or "" for a declaration outside any function (a package variable).
type namedIn struct {
	file     string
	line     int
	function string
}

// where is the place as a guard lists it, after label: "<file>:<line><label> in <func>", or
// "<file>:<line><label> at package level".
func (n namedIn) where(label string) string {
	if n.function == "" {
		return fmt.Sprintf("%s:%d%s at package level", n.file, n.line, label)
	}
	return fmt.Sprintf("%s:%d%s in %s", n.file, n.line, label, n.function)
}

// namesIn parses each non-test Go file of dir and walks every declaration, inside functions and
// outside them, for each use of the identifier name: a call, a value taken into a variable, any
// reference. The identifier a declaration declares is no use of it.
func namesIn(t *testing.T, dir, name string) []namedIn {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := token.NewFileSet()
	var found []namedIn
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(files, filepath.Join(dir, file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			function, declared := "", map[*ast.Ident]bool{}
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				function, declared[decl.Name] = decl.Name.Name, true
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						declared[spec.Name] = true
					case *ast.ValueSpec:
						for _, ident := range spec.Names {
							declared[ident] = true
						}
					}
				}
			}
			ast.Inspect(decl, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok && ident.Name == name && !declared[ident] {
					found = append(found, namedIn{file: file, line: files.Position(ident.Pos()).Line, function: function})
				}
				return true
			})
		}
	}
	return found
}

// usesOutside is every use of name in dir (namesIn) outside the functions allowed names, listed
// as "<file>:<line> in <func>" (namedIn.where), whether it calls, reads or builds name, and how
// many uses each function of allowed holds.
func usesOutside(t *testing.T, dir, name string, allowed map[string]int) (outside []string, inside map[string]int) {
	t.Helper()
	inside = map[string]int{}
	for _, use := range namesIn(t, dir, name) {
		if _, ok := allowed[use.function]; ok {
			inside[use.function]++
			continue
		}
		outside = append(outside, use.where(""))
	}
	return outside, inside
}

// wantOnlyIn fails unless name is named in dir exactly as allowed says, and nowhere else. A guard
// that found no use at all fails too, since allowed names at least one.
func wantOnlyIn(t *testing.T, dir, name string, allowed map[string]int) {
	t.Helper()
	outside, inside := usesOutside(t, dir, name, allowed)
	if len(outside) != 0 || !maps.Equal(inside, allowed) {
		t.Fatalf("%s is named %v in its owners and outside them at %v, want %v and nowhere else", name, inside, outside, allowed)
	}
}

// commitHooksKeyOwners are the functions that name the key a commit scope lives under:
// TransactWith stores the one scope that holds hooks, scopeOf reads it, and readOutcome and
// ForConcurrentWorker store none, so an outcome read and a worker never join a transaction.
var commitHooksKeyOwners = map[string]int{"TransactWith": 1, "scopeOf": 1, "readOutcome": 1, "ForConcurrentWorker": 1}

// noCommitGraceOwners are the functions that name the outcome read's mark: readOutcome sets it,
// and commitContext reads it.
var noCommitGraceOwners = map[string]int{"readOutcome": 1, "commitContext": 1}

// TransactWith is the one place a commit scope opens, so a hook can only wait on a commit whose
// outcome TransactWith reads. Every scope lives under commitHooksKey, and the key's type is the
// package's own, so a scope stored anywhere else names the key there and is listed.
func TestOnlyTransactOpensACommitScope(t *testing.T) {
	t.Parallel()
	wantOnlyIn(t, ".", "commitHooksKey", commitHooksKeyOwners)
}

// The guards' control, an owner parsed and never built: one function that stores a scope under
// the key, one that takes the key's type into a package-level alias, and one that sets the
// outcome read's mark. Each use is listed.
func TestTheScopeGuardsListEveryPlantedUse(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("testdata", "plantedscope")
	for _, tc := range []struct {
		name    string
		allowed map[string]int
		want    []string
	}{
		{"commitHooksKey", commitHooksKeyOwners, []string{"owner.go:9 in plantedOwner", "owner.go:14 at package level"}},
		{"noCommitGrace", noCommitGraceOwners, []string{"owner.go:19 in plantedGrace"}},
	} {
		if outside, _ := usesOutside(t, dir, tc.name, tc.allowed); !slices.Equal(outside, tc.want) {
			t.Errorf("the guard of %s listed %v in the planted owner, want %v", tc.name, outside, tc.want)
		}
	}
}

// Only the outcome read gives its COMMIT and its rollback no grace: noCommitGrace, its mark, is
// named only where readOutcome sets it and where commitContext reads it.
func TestOnlyTheOutcomeReadWaivesTheCommitGrace(t *testing.T) {
	t.Parallel()
	wantOnlyIn(t, ".", "noCommitGrace", noCommitGraceOwners)
}
