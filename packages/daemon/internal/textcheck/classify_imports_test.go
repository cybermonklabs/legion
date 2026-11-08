package textcheck

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestClassifyImportsArePure protects internal/classify, the pure decision package, from gaining
// transport, clock, database, or workflow dependencies.
func TestClassifyImportsArePure(t *testing.T) {
	packageDirectory := filepath.Join("..", "classify")
	files, err := filepath.Glob(filepath.Join(packageDirectory, "*.go"))
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	imports := map[string]bool{}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse imports in %s: %v", path, err)
		}
		for _, importSpec := range file.Imports {
			imports[strings.Trim(importSpec.Path.Value, `\"`)] = true
		}
	}
	allowed := map[string]bool{
		"sort": true,
		"time": true,
		"github.com/sjawhar/legion/daemon/internal/record": true,
	}
	for imported := range imports {
		if !allowed[imported] {
			t.Errorf("the classify package imports %q; classifiers must be pure", imported)
		}
	}
	if !imports["time"] || !imports["github.com/sjawhar/legion/daemon/internal/record"] {
		t.Error("the classify package must retain its record and time contracts")
	}
}
