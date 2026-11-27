package claim

//ci:reads packages/envoy/internal/contracts/generated.go TestTheProjectKeyPatternIsTheContractsOwn parses its ProjectKeyPattern

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// ProjectKeyPattern is the contract's PROJECT_KEY_PATTERN
// (packages/contracts/src/dispatch-tools.ts) character for character. This module cannot import
// the contract's Go, so the test reads the constant the contract's generator writes from it
// (packages/contracts/scripts/gen-go.ts) into Envoy's contracts package, ProjectKeyPattern in
// packages/envoy/internal/contracts/generated.go: a change of the daemon's pattern or of the
// generated one fails here until the other follows.
func TestTheProjectKeyPatternIsTheContractsOwn(t *testing.T) {
	path := filepath.Join("..", "..", "..", "envoy", "internal", "contracts", "generated.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse the contract's generated Go: %v", err)
	}
	var generated []string
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value := spec.(*ast.ValueSpec)
			for i, name := range value.Names {
				if name.Name != "ProjectKeyPattern" {
					continue
				}
				if len(value.Values) != len(value.Names) {
					t.Fatalf("%s declares ProjectKeyPattern with no value of its own", path)
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("%s declares ProjectKeyPattern as %T, want a string literal", path, value.Values[i])
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote the ProjectKeyPattern of %s: %v", path, err)
				}
				generated = append(generated, pattern)
			}
		}
	}
	if len(generated) != 1 || generated[0] != ProjectKeyPattern {
		t.Fatalf("%s declares ProjectKeyPattern as %q, want one constant, the daemon's %q", path, generated, ProjectKeyPattern)
	}
}
