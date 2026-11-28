package backends_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
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

// module answers the path and the root directory of the module this package is in.
func module(t *testing.T) (path, root string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.Module.Path}} {{.Module.Dir}}", ".").Output()
	if err != nil {
		t.Fatalf("go list .: %v", err)
	}
	path, root, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !ok || path == "" || root == "" {
		t.Fatalf("UNKNOWN: go list answered %q, no module path and root", out)
	}
	return path, root
}

// under reports whether path is prefix or a package below it.
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// testSupport reports whether path is a test-support package: one whose last path element ends in
// "test" (awstest, gcptest, policytest, secretstoretest, ciphertest, …).
func testSupport(path string) bool {
	return strings.HasSuffix(path[strings.LastIndex(path, "/")+1:], "test")
}

// goList runs go list with args from the module's root and answers its output's lines.
func goList(t *testing.T, root string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("go list %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// TestEachStoreStaysInItsPackage (c-store-boundary): no package of the module but a store's own
// package and those under it, the registry and the test-support packages imports that store's
// package or one of its ServiceSDKs, so a store's code and its SDK enter a binary through the
// registry alone, and a build tag that leaves the store out leaves them out.
func TestEachStoreStaysInItsPackage(t *testing.T) {
	modulePath, root := module(t)
	all := backends.All()
	if len(all) == 0 {
		t.Fatal("the registry holds no store")
	}
	registry := modulePath + "/internal/cipher/secretstore/backends"
	lines := goList(t, root, "-f", "{{.ImportPath}} {{join .Imports \" \"}}", "./...")
	if len(lines) < 10 {
		t.Fatalf("go list listed %d packages of the module; the walk read nothing", len(lines))
	}
	for _, b := range all {
		own := reflect.TypeOf(b).PkgPath()
		forbidden := append([]string{own}, b.ServiceSDKs()...)
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			pkg := fields[0]
			if under(pkg, own) || pkg == registry || testSupport(pkg) {
				continue
			}
			for _, imp := range fields[1:] {
				for _, f := range forbidden {
					if under(imp, f) {
						t.Errorf("%s imports %s, which only the %s store may import", pkg, imp, b.ID())
					}
				}
			}
		}
	}
}

// TestNoBinaryLinksATestSupportPackage: neither binary that carries a store (cipherd, agent-secrets)
// links a test-support package, whose fakes stand in for a store.
func TestNoBinaryLinksATestSupportPackage(t *testing.T) {
	modulePath, root := module(t)
	deps := goList(t, root, "-deps", "./cmd/cipherd", "./cmd/cipher")
	if len(deps) < 10 {
		t.Fatalf("go list -deps listed %d packages; the walk read nothing", len(deps))
	}
	for _, dep := range deps {
		if under(dep, modulePath) && testSupport(dep) {
			t.Errorf("a binary links %s, a test-support package", dep)
		}
	}
}
