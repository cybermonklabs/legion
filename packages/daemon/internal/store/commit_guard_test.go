package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// usesOutside is every use of name in dir (namesIn), listed after label (namedIn.where): one
// inside a function declaration named owner counts in inside, so a second use there, or a method
// of that name, raises inside past the one the guards admit; any other is listed, whether it calls
// name or takes it as a value, since a value taken anywhere can be called anywhere.
func usesOutside(t *testing.T, dir, name, owner, label string) (outside []string, inside int) {
	t.Helper()
	for _, use := range namesIn(t, dir, name) {
		if use.function == owner {
			inside++
			continue
		}
		outside = append(outside, use.where(label))
	}
	return outside, inside
}

// scopeOpenersOutsideTransact is every use of withCommitHooks in dir outside TransactWith, as
// "<file>:<line> in <func>", and the count of those inside it (usesOutside).
func scopeOpenersOutsideTransact(t *testing.T, dir string) (outside []string, inside int) {
	t.Helper()
	return usesOutside(t, dir, "withCommitHooks", "TransactWith", "")
}

// TransactWith is the one place a commit scope opens, so a hook can only wait on a commit whose
// outcome TransactWith reads.
func TestOnlyTransactOpensACommitScope(t *testing.T) {
	outside, inside := scopeOpenersOutsideTransact(t, ".")
	if inside == 0 && len(outside) == 0 {
		t.Fatal("the guard found no scope opener at all")
	}
	if len(outside) != 0 || inside != 1 {
		t.Fatalf("withCommitHooks is named %d times in TransactWith and outside it at %v, want once in TransactWith and nowhere else", inside, outside)
	}
}

// outcomeReadsOutsideReadOutcome is every use of outcomeReadContext in dir outside readOutcome,
// as "<file>:<line> outcomeReadContext in <func>", and the count of those inside it
// (usesOutside).
func outcomeReadsOutsideReadOutcome(t *testing.T, dir string) (outside []string, inside int) {
	t.Helper()
	return usesOutside(t, dir, "outcomeReadContext", "readOutcome", " outcomeReadContext")
}

// The guards' controls, each an owner parsed and never built: one that calls each helper outside
// its owner, one that takes each into a local variable and calls it through that, and one that
// takes each into a package variable and calls it through that. Each use is listed.
func TestTheScopeGuardsListEveryPlantedUse(t *testing.T) {
	for _, tc := range []struct {
		dir            string
		scope, outcome []string
	}{
		{dir: "direct", scope: []string{"owner.go:9 in plantedOwner"},
			outcome: []string{"owner.go:10 outcomeReadContext in plantedOwner"}},
		{dir: "localalias", scope: []string{"owner.go:8 in plantedOwner"},
			outcome: []string{"owner.go:10 outcomeReadContext in plantedOwner"}},
		{dir: "packagealias", scope: []string{"owner.go:8 at package level"},
			outcome: []string{"owner.go:9 outcomeReadContext at package level"}},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			dir := filepath.Join("testdata", "plantedscope", tc.dir)
			if outside, _ := scopeOpenersOutsideTransact(t, dir); !slices.Equal(outside, tc.scope) {
				t.Errorf("the scope guard listed %v in the planted owner, want %v", outside, tc.scope)
			}
			if outside, _ := outcomeReadsOutsideReadOutcome(t, dir); !slices.Equal(outside, tc.outcome) {
				t.Errorf("the outcome guard listed %v in the planted owner, want %v", outside, tc.outcome)
			}
		})
	}
}

// Only the outcome read gives its COMMIT and its rollback no grace: readOutcome is the one place
// that names outcomeReadContext, and noCommitGrace, the mark it sets, is named only where it is
// set and where commitContext reads it.
func TestOnlyTheOutcomeReadWaivesTheCommitGrace(t *testing.T) {
	outside, inside := outcomeReadsOutsideReadOutcome(t, ".")
	if inside == 0 && len(outside) == 0 {
		t.Fatal("the guard found no outcome read")
	}
	if len(outside) != 0 || inside != 1 {
		t.Fatalf("outcomeReadContext is named %d times in readOutcome and outside it at %v, want once in readOutcome and nowhere else", inside, outside)
	}
	var marks []string
	for _, use := range namesIn(t, ".", "noCommitGrace") {
		if use.function != "outcomeReadContext" && use.function != "commitContext" {
			marks = append(marks, use.where(""))
		}
	}
	if len(marks) != 0 {
		t.Fatalf("noCommitGrace is named at %v, want only in outcomeReadContext and commitContext", marks)
	}
}
