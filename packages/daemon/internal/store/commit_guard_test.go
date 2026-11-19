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

// scopeOpenersOutsideTransact parses each non-test Go file of dir and walks every function
// declaration for calls of withCommitHooks: a call inside a declaration named Transact counts in
// inside, so a second such call, in a method of that name too, raises inside past the one
// TestOnlyTransactOpensACommitScope admits; any other is listed as "<file>:<line> in <func>".
func scopeOpenersOutsideTransact(t *testing.T, dir string) (outside []string, inside int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(function, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callee, ok := call.Fun.(*ast.Ident); !ok || callee.Name != "withCommitHooks" {
					return true
				}
				if function.Name.Name == "Transact" {
					inside++
					return true
				}
				outside = append(outside, fmt.Sprintf("%s:%d in %s", name, files.Position(call.Pos()).Line, function.Name.Name))
				return true
			})
		}
	}
	return outside, inside
}

// Transact is the one place a commit scope opens, so a hook can only wait on a commit whose
// outcome Transact reads.
func TestOnlyTransactOpensACommitScope(t *testing.T) {
	outside, inside := scopeOpenersOutsideTransact(t, ".")
	if inside == 0 && len(outside) == 0 {
		t.Fatal("the guard found no scope opener at all")
	}
	if len(outside) != 0 || inside != 1 {
		t.Fatalf("withCommitHooks is called %d times in Transact and outside it at %v, want once in Transact and nowhere else", inside, outside)
	}
}

// The guard's control: an owner that opens a scope of its own, parsed and never built.
func TestTheScopeGuardFindsAPlantedOpener(t *testing.T) {
	outside, _ := scopeOpenersOutsideTransact(t, filepath.Join("testdata", "plantedscope"))
	if want := []string{"owner.go:7 in plantedOwner"}; !slices.Equal(outside, want) {
		t.Fatalf("the guard listed %v in the planted owner, want %v", outside, want)
	}
}
