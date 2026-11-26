package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// goFunction is the byte range of the function named name (types.Func.FullName's form) in a Go
// file of the snapshot at root, found by syntax alone.
func goFunction(data []byte, root, file, name string) ([2]int, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, data, parser.SkipObjectResolution)
	if err != nil {
		return [2]int{}, false
	}
	path, ok := goImportPath(root, file)
	if !ok {
		return [2]int{}, false
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && goFuncName(path, fd) == name {
			return [2]int{fset.Position(fd.Pos()).Offset, fset.Position(fd.End()).Offset}, true
		}
	}
	return [2]int{}, false
}

// goImportPath is the import path of the package a Go file of the snapshot at root is in: the
// path its module's go.mod names (the nearest one above the file: packages/envoy's, the daemon's,
// a nested module's), followed by the file's directory under that module. A file under no go.mod
// has none.
func goImportPath(root, file string) (string, bool) {
	dir := filepath.Dir(filepath.FromSlash(file))
	for mod := dir; mod != "." && mod != string(filepath.Separator); mod = filepath.Dir(mod) {
		data, err := os.ReadFile(filepath.Join(root, mod, "go.mod"))
		if err != nil {
			continue
		}
		path := modfileModule(data)
		if path == "" {
			return "", false
		}
		rel, err := filepath.Rel(mod, dir)
		if err != nil {
			return "", false
		}
		if rel == "." {
			return path, true
		}
		return path + "/" + filepath.ToSlash(rel), true
	}
	return "", false
}

// modfileModule is the module path a go.mod's module directive names, or "".
func modfileModule(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// goFuncName is types.Func.FullName of fd, from syntax: "pkg.F", "(pkg.T).M" or "(*pkg.T).M".
func goFuncName(path string, fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return path + "." + fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	star := ""
	if s, ok := t.(*ast.StarExpr); ok {
		star, t = "*", s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if ix, ok := t.(*ast.IndexListExpr); ok {
		t = ix.X
	}
	id, _ := t.(*ast.Ident)
	if id == nil {
		return path + ".?." + fd.Name.Name
	}
	return "(" + star + path + "." + id.Name + ")." + fd.Name.Name
}
