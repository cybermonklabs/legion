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

// namedIn is one place a function declaration of dir names an identifier: the file, the line and
// the declaration's name, and whether the identifier is the callee of a call there.
type namedIn struct {
	file     string
	line     int
	function string
	called   bool
}

// namesIn parses each non-test Go file of dir and walks every function declaration for the
// identifier name, as a callee or as any other use. Declarations outside functions (a type, a
// variable) are not walked.
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
			function, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			callees := map[*ast.Ident]bool{}
			ast.Inspect(function, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if callee, ok := call.Fun.(*ast.Ident); ok {
						callees[callee] = true
					}
				}
				if ident, ok := node.(*ast.Ident); ok && ident.Name == name && ident != function.Name {
					found = append(found, namedIn{file: file, line: files.Position(ident.Pos()).Line,
						function: function.Name.Name, called: callees[ident]})
				}
				return true
			})
		}
	}
	return found
}

// scopeOpenersOutsideTransact is every call of withCommitHooks in dir: a call inside a
// declaration named TransactWith counts in inside, so a second such call, in a method of that
// name too, raises inside past the one TestOnlyTransactOpensACommitScope admits; any other is
// listed as "<file>:<line> in <func>".
func scopeOpenersOutsideTransact(t *testing.T, dir string) (outside []string, inside int) {
	t.Helper()
	for _, use := range namesIn(t, dir, "withCommitHooks") {
		switch {
		case !use.called:
		case use.function == "TransactWith":
			inside++
		default:
			outside = append(outside, fmt.Sprintf("%s:%d in %s", use.file, use.line, use.function))
		}
	}
	return outside, inside
}

// TransactWith is the one place a commit scope opens, so a hook can only wait on a commit whose
// outcome TransactWith reads.
func TestOnlyTransactOpensACommitScope(t *testing.T) {
	outside, inside := scopeOpenersOutsideTransact(t, ".")
	if inside == 0 && len(outside) == 0 {
		t.Fatal("the guard found no scope opener at all")
	}
	if len(outside) != 0 || inside != 1 {
		t.Fatalf("withCommitHooks is called %d times in TransactWith and outside it at %v, want once in TransactWith and nowhere else", inside, outside)
	}
}

// The guard's control: an owner that opens a scope of its own, parsed and never built.
func TestTheScopeGuardFindsAPlantedOpener(t *testing.T) {
	outside, _ := scopeOpenersOutsideTransact(t, filepath.Join("testdata", "plantedscope"))
	if want := []string{"owner.go:9 in plantedOwner"}; !slices.Equal(outside, want) {
		t.Fatalf("the guard listed %v in the planted owner, want %v", outside, want)
	}
}

// outcomeReadsOutsideReadOutcome is every call of outcomeReadContext in dir: one inside
// readOutcome counts in inside; any other is listed as "<file>:<line> outcomeReadContext in
// <func>".
func outcomeReadsOutsideReadOutcome(t *testing.T, dir string) (outside []string, inside int) {
	t.Helper()
	for _, use := range namesIn(t, dir, "outcomeReadContext") {
		switch {
		case !use.called:
		case use.function == "readOutcome":
			inside++
		default:
			outside = append(outside, fmt.Sprintf("%s:%d outcomeReadContext in %s", use.file, use.line, use.function))
		}
	}
	return outside, inside
}

// Only the outcome read gives its COMMIT and its rollback no grace: readOutcome is the one caller
// of outcomeReadContext, and noCommitGrace, the mark it sets, is named only where it is set and
// where commitContext reads it.
func TestOnlyTheOutcomeReadWaivesTheCommitGrace(t *testing.T) {
	outside, inside := outcomeReadsOutsideReadOutcome(t, ".")
	if inside == 0 && len(outside) == 0 {
		t.Fatal("the guard found no outcome read")
	}
	if len(outside) != 0 || inside != 1 {
		t.Fatalf("outcomeReadContext is called %d times in readOutcome and outside it at %v, want once in readOutcome and nowhere else", inside, outside)
	}
	var marks []string
	for _, use := range namesIn(t, ".", "noCommitGrace") {
		if use.function != "outcomeReadContext" && use.function != "commitContext" {
			marks = append(marks, fmt.Sprintf("%s:%d in %s", use.file, use.line, use.function))
		}
	}
	if len(marks) != 0 {
		t.Fatalf("noCommitGrace is named at %v, want only in outcomeReadContext and commitContext", marks)
	}
	planted, _ := outcomeReadsOutsideReadOutcome(t, filepath.Join("testdata", "plantedscope"))
	if want := []string{"owner.go:10 outcomeReadContext in plantedOwner"}; !slices.Equal(planted, want) {
		t.Fatalf("the guard listed %v in the planted owner, want %v", planted, want)
	}
}
