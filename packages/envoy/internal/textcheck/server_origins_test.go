package textcheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// textImport is the package that holds text.Origins, the list of origins every reader of a
// Dispatch link reads through.
const textImport = "github.com/sjawhar/envoy/internal/dispatch/text"

// serverOriginsConstructor is the one function of package text that builds the server's list of
// origins, from the canonical origin and the served and former lists (text.ServerOrigins).
const serverOriginsConstructor = "ServerOrigins"

// The server builds its list of origins in one place, text.ServerOrigins: cmd/dispatch's main and
// rebuild-refs and api.NewDeps each call it, so the reference index, the document service, the
// API's URL parameters, rebuild-refs and the result line read one list, and a served or a former
// address added to it is read by every one of them. A list built anywhere else is a reader that
// misses an address: a call of text.NewOrigins in a non-test file of cmd/dispatch, cmd/dispatch-seed
// or internal/dispatch, anywhere but in the constructor, fails here, naming its file and
// declaration.
func TestOnlyOneConstructorBuildsTheServersOrigins(t *testing.T) {
	builds, read, err := originsBuilds(
		filepath.Join("..", "..", "cmd", "dispatch"),
		filepath.Join("..", "..", "cmd", "dispatch-seed"),
		filepath.Join("..", "dispatch"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if read == 0 {
		t.Fatal("read no Go file: the check cannot see the server's code")
	}
	if len(builds) > 0 {
		t.Errorf("%d declarations build a list of origins beside text.%s:\n\t%s\n"+
			"build it with text.%s(canonical, served, former), so every reader of a Dispatch link reads the served and former addresses too",
			len(builds), serverOriginsConstructor, strings.Join(builds, "\n\t"), serverOriginsConstructor)
	}

	// The check sees a build however the file names it: under an imported name of its own, in a
	// function literal, in a package-level variable, as a bare NewOrigins inside package text
	// anywhere but in the constructor, and through a dot import, which is the finding itself. The
	// constructor's own call is not one.
	t.Run("the check sees what it guards", func(t *testing.T) {
		root := t.TempDir()
		planted := map[string]string{
			filepath.Join("seed", "seed.go"): "package seed\n\nimport links \"" + textImport + "\"\n\n" +
				"var origins = links.NewOrigins(\"https://a.example.test\")\n\n" +
				"func build() links.Origins {\n\tread := func() links.Origins { return links.NewOrigins(\"https://a.example.test\") }\n\treturn read()\n}\n",
			filepath.Join("dot", "dot.go"): "package dot\n\nimport . \"" + textImport + "\"\n\nvar origins = Origins{}\n",
			filepath.Join("dispatch", "text", "origins.go"): "package text\n\ntype Origins struct{}\n\n" +
				"func NewOrigins(canonical string, others ...string) Origins { return Origins{} }\n\n" +
				"func ServerOrigins(canonical string, served, former []string) Origins { return NewOrigins(canonical) }\n\n" +
				"func other() Origins { return NewOrigins(\"https://a.example.test\") }\n",
		}
		for name, source := range planted {
			path := filepath.Join(root, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, read, err := originsBuilds(root)
		if err != nil {
			t.Fatal(err)
		}
		at := filepath.ToSlash(root)
		want := []string{
			at + "/dispatch/text/origins.go other",
			at + "/dot/dot.go import . " + textImport,
			at + "/seed/seed.go build",
			at + "/seed/seed.go var origins",
		}
		if read != len(planted) || !slices.Equal(got, want) {
			t.Errorf("the check read %d files and found\n\t%s\nwant %d files and\n\t%s", read, strings.Join(got, "\n\t"), len(planted), strings.Join(want, "\n\t"))
		}
	})
}

// originsBuilds reads the non-test Go files under roots (goFiles) and returns, sorted, each
// top-level declaration that names text.NewOrigins, as "<path> <declaration>" (declName), whatever
// name the file imports text under; inside package text itself (a file of package text in a
// directory dispatch/text) a bare NewOrigins, except in the constructor, ServerOrigins; and each dot
// import of text, as "<path> import . <text>", since a bare NewOrigins through it cannot be told
// from a function of the file's own. It counts the Go files it read.
func originsBuilds(roots ...string) (builds []string, read int, err error) {
	read, err = goFiles(roots, false, func(path string, file *ast.File, _ *token.FileSet) {
		at := filepath.ToSlash(path)
		inText := file.Name.Name == "text" && strings.HasSuffix(filepath.ToSlash(filepath.Dir(path)), "dispatch/text")
		names := importNames(file, textImport)
		if names["."] {
			builds = append(builds, at+" import . "+textImport)
		}
		for _, decl := range file.Decls {
			if function, ok := decl.(*ast.FuncDecl); ok && inText && function.Recv == nil && function.Name.Name == serverOriginsConstructor {
				continue
			}
			if namesNewOrigins(decl, names, inText) {
				builds = append(builds, at+" "+declName(decl))
			}
		}
	})
	if err != nil {
		return nil, read, err
	}
	slices.Sort(builds)
	return builds, read, nil
}

// namesNewOrigins reports whether decl names NewOrigins: as a selector on one of names, the names
// its file imports text under, or, in package text itself (inText), as a bare identifier other
// than the name NewOrigins is declared under.
func namesNewOrigins(decl ast.Decl, names map[string]bool, inText bool) bool {
	var declared *ast.Ident
	if function, ok := decl.(*ast.FuncDecl); ok {
		declared = function.Name
	}
	found := false
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := node.X.(*ast.Ident); ok && names[pkg.Name] && node.Sel.Name == "NewOrigins" {
				found = true
			}
			// Sel names a field, a method or an imported package's member, never package text's
			// own bare NewOrigins, so only X is read on.
			ast.Inspect(node.X, visit)
			return false
		case *ast.Ident:
			if inText && node != declared && node.Name == "NewOrigins" {
				found = true
			}
		}
		return !found
	}
	ast.Inspect(decl, visit)
	return found
}

// declName is how these checks name a top-level declaration: a function by its name, a method as
// (receiver type).name, and a variable or a constant as "var" or "const" and its names.
func declName(decl ast.Decl) string {
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		if decl.Recv != nil && len(decl.Recv.List) > 0 {
			return "(" + types.ExprString(decl.Recv.List[0].Type) + ")." + decl.Name.Name
		}
		return decl.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					names = append(names, name.Name)
				}
			case *ast.TypeSpec:
				names = append(names, spec.Name.Name)
			}
		}
		return decl.Tok.String() + " " + strings.Join(names, ", ")
	}
	return "?"
}
