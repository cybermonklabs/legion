package textcheck

//ci:reads packages/daemon/**/*.go TestEveryCreatedDatabaseRunsWithoutJIT walks every .go file under packages/daemon (../..)

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// createdWithoutJITExempt names the functions that create a database and leave JIT as the server
// has it, by file and function, each with why.
var createdWithoutJITExempt = map[string]string{
	"internal/launcher/testpg.go CheckTestPostgres": "the pod check of the test server creates a probe database " +
		"and drops it at once, running no statement in it",
}

// Every database a Go file of this module creates runs with JIT off, as a Cloud SQL store's does
// (docs/testing.md, "A database a Go test creates"): a function whose body holds a string literal
// that opens a database's creation also calls WithoutJIT (internal/testpg/testdb) on it. The check
// reads every Go file of the module, tests included, so a new helper written without the call, or
// an edit that drops one, fails here naming its file and function, with no database needed. A
// function that only returns the statement (a clone's cloneStatement) is a builder, and its
// caller's database is read by testpg's TestEveryDatabaseRunsWithoutJIT.
func TestEveryCreatedDatabaseRunsWithoutJIT(t *testing.T) {
	if offences := creationsWithJIT(t, "../..", createdWithoutJITExempt); len(offences) > 0 {
		t.Fatalf("%d functions create a database and leave JIT on:\n\t%s\n"+
			"run testdb.WithoutJIT on each database the function creates (docs/testing.md, \"A database a Go test creates\")",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// creationsWithJIT names, as file:line function, every function under root whose body holds a
// string literal opening a database's creation and no call of a function named WithoutJIT,
// leaving out builders (a body of one return statement) and the exempt functions, keyed by their
// file relative to root and their name. Comments are not literals, so prose is not read.
func creationsWithJIT(t *testing.T, root string, exempt map[string]string) []string {
	t.Helper()
	opening := "create" + " database" // written apart so this file holds no opening of its own
	var offences []string
	used := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			var creates token.Pos
			calls := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.BasicLit:
					if node.Kind == token.STRING && !creates.IsValid() {
						text, err := strconv.Unquote(node.Value)
						if err != nil {
							text = node.Value
						}
						if strings.Contains(strings.ToLower(text), opening) {
							creates = node.Pos()
						}
					}
				case *ast.CallExpr:
					switch callee := node.Fun.(type) {
					case *ast.Ident:
						calls = calls || callee.Name == "WithoutJIT"
					case *ast.SelectorExpr:
						calls = calls || callee.Sel.Name == "WithoutJIT"
					}
				}
				return true
			})
			if !creates.IsValid() || calls {
				continue
			}
			if _, builder := function.Body.List[0].(*ast.ReturnStmt); builder && len(function.Body.List) == 1 {
				continue
			}
			key := filepath.ToSlash(relative) + " " + function.Name.Name
			if _, ok := exempt[key]; ok {
				used[key] = true
				continue
			}
			offences = append(offences, filepath.ToSlash(path)+":"+strconv.Itoa(files.Position(creates).Line)+" "+function.Name.Name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files: %v", err)
	}
	for key := range exempt {
		if !used[key] {
			offences = append(offences, key+": exempt, but it creates no database with JIT on any more; take it out of the exemptions")
		}
	}
	slices.Sort(offences)
	return offences
}
