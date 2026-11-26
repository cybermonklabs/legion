package independent

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// module is this module's import path, and self this package's: the reading and its command
// import no package of the module but this one.
const (
	module = "github.com/sjawhar/envoy/"
	self   = module + "internal/dispatch/evals/independent"
)

// computations are the packages that compute what this reading is compared with, named so a
// failure says which one the reading reached.
var computations = []string{
	module + "internal/dispatch/legion/metrics",
	module + "internal/dispatch/legionfacts",
	module + "internal/dispatch/evals",
}

// TestTheReadingImportsNothingItIsComparedWith reads the import list of every file of the package
// and of its command that is not a test, and fails on any package of this module but the reading
// itself: a reading that imported legion/metrics, legionfacts or evals, or a constant of a package
// they read, would share the rules it checks.
func TestTheReadingImportsNothingItIsComparedWith(t *testing.T) {
	var sources []string
	for _, pattern := range []string{"*.go", filepath.Join("cmd", "*.go")} {
		matched, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("list %s: %v", pattern, err)
		}
		sources = append(sources, matched...)
	}
	files, imports := 0, 0
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), source, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", source, err)
		}
		files++
		for _, spec := range parsed.Imports {
			imports++
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s imports %s, which is no quoted path: %v", source, spec.Path.Value, err)
			}
			if !strings.HasPrefix(path, module) || path == self {
				continue
			}
			named := "a package of this module"
			for _, computation := range computations {
				if path == computation || strings.HasPrefix(path, computation+"/") {
					named = "the computation it is compared with"
				}
			}
			t.Errorf("%s imports %s, %s", source, path, named)
		}
	}
	t.Logf("read %d imports of %d files", imports, files)
	if files < 2 || imports == 0 {
		t.Fatalf("read %d imports of %d files, and a check that read none proves nothing", imports, files)
	}
}
