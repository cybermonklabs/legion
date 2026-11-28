package backends_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
)

// TestTheRegistryHoldsAStore: the registry holds at least one store, sorted by name, Lookup answers
// each by its name, and a name it does not hold is refused naming every name it does.
func TestTheRegistryHoldsAStore(t *testing.T) {
	all := backends.All()
	if len(all) == 0 {
		t.Fatal("the registry holds no store")
	}
	var names []string
	for _, b := range all {
		names = append(names, b.Name())
		got, err := backends.Lookup(b.Name())
		if err != nil || got.ID() != b.ID() {
			t.Errorf("Lookup(%s) = %v, %v; want the %s store", b.Name(), got, err, b.ID())
		}
	}
	if !slices.IsSorted(names) {
		t.Errorf("All() = %v, want it sorted by name", names)
	}
	_, err := backends.Lookup("example-store")
	want := `BROKER_SECRETS_STORE must be one of ` + strings.Join(names, ", ") + `, got "example-store"`
	if err == nil || err.Error() != want {
		t.Fatalf("Lookup(example-store) = %v, want %q", err, want)
	}
}

// TestEachRegistryFileIsBehindItsStoresTag: every file of the registry but backends.go starts with
// the build line that leaves its store out (//go:build !cipher_no_<file base>) and registers, with
// var _ = register(<package>.Backend{}), a store whose package is named for the file and whose ID
// is the file's base, so one tag removes one store with its SDK.
func TestEachRegistryFileIsBehindItsStoresTag(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, b := range backends.All() {
		registered[b.ID()] = true
	}
	checked := 0
	for _, file := range files {
		if file == "backends.go" || strings.HasSuffix(file, "_test.go") {
			continue
		}
		checked++
		id := strings.TrimSuffix(file, ".go")
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if len(parsed.Comments) == 0 || parsed.Comments[0].Pos() != parsed.FileStart || parsed.Comments[0].List[0].Text != "//go:build !cipher_no_"+id {
			t.Errorf("%s does not start with //go:build !cipher_no_%s", file, id)
		}
		imports := map[string]string{}
		for _, spec := range parsed.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			name := importPath[strings.LastIndex(importPath, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = importPath
		}
		var registers []string
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "register" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
				if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Backend" {
					if pkg, ok := sel.X.(*ast.Ident); ok {
						registers = append(registers, imports[pkg.Name])
					}
				}
			}
			return true
		})
		if len(registers) != 1 || !strings.HasSuffix(registers[0], "/secretstore/"+id) {
			t.Errorf("%s registers %v; want one register(%s.Backend{}) of the package secretstore/%s", file, registers, id, id)
		}
		if !registered[id] {
			t.Errorf("%s: no store in All() has the ID %s", file, id)
		}
	}
	if checked == 0 {
		t.Fatal("UNKNOWN: the registry directory holds no store file")
	}
}
