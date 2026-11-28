package cmdtest

import (
	"go/ast"
	"go/token"
)

// ConstExpr is the expression the constant named name is declared with in file, or nil when the
// file declares no constant of that name with a value of its own. A spec that carries no value - a
// line of a const block that repeats the one above it, as an iota run does - is no expression to
// evaluate, so it reads as the name being absent rather than as the previous line's. A command's
// test that holds a number the infra module copies by hand reads the copy with it: that module
// (packages/envoy/infra) is a Go module of its own, which no command imports, so the test parses
// its source.
func ConstExpr(file *ast.File, name string) ast.Expr {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range valueSpec.Names {
				if ident.Name == name && i < len(valueSpec.Values) {
					return valueSpec.Values[i]
				}
			}
		}
	}
	return nil
}
