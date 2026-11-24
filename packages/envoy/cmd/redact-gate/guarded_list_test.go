package main

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// valueMethods are the exported methods of the toolchain's own reflect.Value, read from its source,
// by name.
func valueMethods(t *testing.T) map[string]*types.Func {
	t.Helper()
	reflect, err := importer.ForCompiler(token.NewFileSet(), "source", nil).Import("reflect")
	if err != nil {
		t.Fatalf("reflect cannot be loaded from source: %v", err)
	}
	methods := map[string]*types.Func{}
	set := types.NewMethodSet(reflect.Scope().Lookup("Value").Type())
	for i := range set.Len() {
		if m := set.At(i).Obj().(*types.Func); m.Exported() {
			methods[m.Name()] = m
		}
	}
	return methods
}

// TestTheGuardedRulesOneMethodListIsEveryMethodItRefuses: guardedPackageRead's statement
// (guarded.go) holds the one list of the methods of reflect.Value that its condition 3 refuses on a
// guarded read's result, the ones whose results are not all closed values (closedValue). Read from
// the toolchain's own reflect, the list names every such method and no other, so a toolchain that
// adds one fails here until the list names it. Control: Bool, String and Kind, which answer closed
// values, are among the methods closedValue passes.
func TestTheGuardedRulesOneMethodListIsEveryMethodItRefuses(t *testing.T) {
	methods := valueMethods(t)
	var open []string
	for name, m := range methods {
		results := m.Type().(*types.Signature).Results()
		for i := range results.Len() {
			if !closedValue(results.At(i).Type()) {
				open = append(open, name)
				break
			}
		}
	}
	slices.Sort(open)
	for _, closed := range []string{"Bool", "String", "Kind"} {
		if methods[closed] == nil || slices.Contains(open, closed) {
			t.Fatalf("control: reflect.Value's %s is not among the methods closedValue passes; the refused ones read: %v", closed, open)
		}
	}
	f, err := parser.ParseFile(token.NewFileSet(), "guarded.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "guardedPackageRead" && fd.Doc != nil {
			doc = strings.Join(strings.Fields(fd.Doc.Text()), " ")
		}
	}
	const from, to = "the one list of them", "an address;"
	start := strings.Index(doc, from)
	end := strings.Index(doc, to)
	if start < 0 || end < start {
		t.Fatalf("guardedPackageRead's statement holds no list from %q to %q:\n%s", from, to, doc)
	}
	var listed []string
	for _, word := range regexp.MustCompile(`\b[A-Z][A-Za-z0-9]*\b`).FindAllString(doc[start:end], -1) {
		if methods[word] != nil && !slices.Contains(listed, word) {
			listed = append(listed, word)
		}
	}
	slices.Sort(listed)
	if !slices.Equal(listed, open) {
		t.Fatalf("guardedPackageRead's list names the reflect.Value methods\n%v\nand closedValue refuses\n%v", listed, open)
	}
}
