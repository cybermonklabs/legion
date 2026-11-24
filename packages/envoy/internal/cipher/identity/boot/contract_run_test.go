package boot_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
)

// TestEachRegisteredProviderRunsTheContract: every identity provider this build registers has a
// test that calls identitytest.Contract in its own package, so a provider added to the registry
// with no contract run fails the build (spec R7). It reads each provider package's test files with
// go/parser, so it sees the call whether or not that package's tests run in the same go test.
func TestEachRegisteredProviderRunsTheContract(t *testing.T) {
	kinds := boot.Kinds()
	if len(kinds) == 0 {
		t.Fatal("boot.Kinds() is empty: the registry holds no provider, so this check enumerated nothing")
	}
	for _, kind := range kinds {
		dir := filepath.Join("..", string(kind))
		tests, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the registry holds %s, and there is no package internal/cipher/identity/%s: %v", kind, kind, err)
			continue
		}
		if !callsContract(t, tests) {
			t.Errorf("the registry holds %s, and no test of internal/cipher/identity/%s calls identitytest.Contract", kind, kind)
		}
	}
}

// callsContract reports whether any of files holds a call of identitytest.Contract.
func callsContract(t *testing.T, files []string) bool {
	t.Helper()
	found := false
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return !found
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return !found
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "identitytest" && selector.Sel.Name == "Contract" {
				found = true
			}
			return !found
		})
	}
	return found
}

// TestEachKindIsRegisteredOnceInRankOrder: the kinds of a build are in rank order, each once, so
// a reader of subjects asks them in one order whatever the build.
func TestEachKindIsRegisteredOnceInRankOrder(t *testing.T) {
	kinds := boot.Kinds()
	seen := map[string]bool{}
	for _, kind := range kinds {
		if seen[string(kind)] {
			t.Errorf("boot.Kinds() = %v names %s twice", kinds, kind)
		}
		seen[string(kind)] = true
	}
	order := map[string]int{"kubernetes": 0, "google": 1, "github": 2}
	for i := 1; i < len(kinds); i++ {
		if order[string(kinds[i-1])] > order[string(kinds[i])] {
			t.Errorf("boot.Kinds() = %v, want rank order kubernetes, google, github", kinds)
		}
	}
}
