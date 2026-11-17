package textcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// storetestImport is the one package under internal/dispatch that reads the process environment
// for itself: the Postgres fixture of Dispatch's tests (DISPATCH_TEST_DATABASE_URL), which only
// test files may import.
const storetestImport = "github.com/sjawhar/envoy/internal/dispatch/store/storetest"

// environmentReaders are the functions that read the process environment, by package.
var environmentReaders = map[string][]string{
	"os":      {"Getenv", "LookupEnv", "Environ", "ExpandEnv"},
	"syscall": {"Getenv", "Environ"},
}

// environmentReads walks the non-test Go files under roots and returns each reference to an
// environment reader as "<path> <package>.<name>", whatever name the file imports the package
// under, and each dot import of a reader's package as "<path> import . <package>": with the
// package's names unqualified, a read cannot be told from a call to the file's own function, so
// the import is the finding. It also returns the files that import storetest.
func environmentReads(roots ...string) (reads, storetestImporters []string, err error) {
	fileSet := token.NewFileSet()
	for _, root := range roots {
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || entry.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			fixture := strings.Contains(filepath.ToSlash(path), "store/storetest/")
			// The name each environment-reading package has in this file.
			packages := map[string]string{}
			for _, spec := range file.Imports {
				importPath, _ := strconv.Unquote(spec.Path.Value)
				if importPath == storetestImport {
					storetestImporters = append(storetestImporters, path)
				}
				if _, reads := environmentReaders[importPath]; !reads {
					continue
				}
				name := importPath
				if spec.Name != nil {
					name = spec.Name.Name
				}
				if name == "." && !fixture {
					reads = append(reads, filepath.ToSlash(path)+" import . "+importPath)
				}
				packages[name] = importPath
			}
			if fixture {
				return nil
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if importPath, ok := packages[pkg.Name]; ok && slices.Contains(environmentReaders[importPath], selector.Sel.Name) {
					position := fileSet.Position(selector.Pos())
					reads = append(reads, filepath.ToSlash(position.Filename)+" "+importPath+"."+selector.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return reads, storetestImporters, nil
}

// writeFile writes contents under a test directory and returns its path.
func writeFile(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Dispatch reads its environment in one place: processSettings, over the settings table. Any
// other reference to the process environment under cmd/dispatch or internal/dispatch is a reader
// the table, `envoy-dispatch settings` and the generated configuration reference do not list.
func TestNoReaderBypassesTheSettingsTable(t *testing.T) {
	reads, storetestImporters, err := environmentReads(filepath.Join("..", "..", "cmd", "dispatch"), filepath.Join("..", "dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"../../cmd/dispatch/settings.go os.LookupEnv"}; !slices.Equal(reads, want) {
		t.Errorf("environment reads outside the settings table: %q, want only %q (processSettings). Add the variable as a row of the settings table (cmd/dispatch/settings.go) and hand its reader the value", reads, want)
	}
	if len(storetestImporters) > 0 {
		t.Errorf("non-test files import %s, which reads DISPATCH_TEST_DATABASE_URL outside the settings table: %q", storetestImport, storetestImporters)
	}
}

// The guard sees a read however the file imports its package: under another name, or with no
// name at all, where the read is a bare Getenv.
func TestTheGuardSeesAReadUnderAnyImportName(t *testing.T) {
	for name, probe := range map[string]string{
		"an aliased import":       "package auth\n\nimport environment \"os\"\n\nfunc acceptProbe() string { return environment.Getenv(\"DISPATCH_ACCEPT_PROBE\") }\n",
		"a dot import":            "package auth\n\nimport . \"os\"\n\nfunc acceptProbe() string { return Getenv(\"DISPATCH_ACCEPT_PROBE\") }\n",
		"a dot import of syscall": "package auth\n\nimport . \"syscall\"\n\nfunc acceptProbe() []string { return Environ() }\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, filepath.Join("auth", "probe.go"), probe)
			reads, _, err := environmentReads(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			if len(reads) != 1 || !strings.HasPrefix(reads[0], filepath.ToSlash(path)+" ") {
				t.Errorf("environment reads %q, want the one in %s", reads, path)
			}
		})
	}
}
