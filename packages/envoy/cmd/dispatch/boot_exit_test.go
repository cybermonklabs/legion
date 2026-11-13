package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// flushingExit is the one function of this package that may call os.Exit: it writes the log lines
// the handler still holds first (main.go's exit).
const flushingExit = "exit"

// Every exit of this process goes through exit(), which writes the buffered log lines before
// os.Exit ends it. os.Exit runs no deferred call and waits for no writer, so a boot step that
// calls it straight after slog.Error ends the process with its own reason still in the handler's
// buffer: a boot failure with no cause, which is the one failure nobody can act on - the process
// stops and its log never says why it would not start. fatal() is that reason logged and written.
//
// main.go states the rule in a comment, and three boot steps - the answerer, the broker
// access-token verifier and the CI reconcile pass - had drifted from it, which is what a comment
// can never catch. So this reads the package's own source and holds the rule to it: no call to
// os.Exit anywhere but inside exit itself.
//
// It parses the source (go/parser) rather than scanning its text because the rule is about calls:
// the characters "os.Exit" in a comment or a string are not one, the call inside exit is the one
// that must stay, and only the parsed tree tells them apart. Test files are left out, as goFilesIn
// leaves them out: TestMain ends a test binary, not a boot.
func TestNoBootPathExitsWithoutWritingItsLogs(t *testing.T) {
	fileSet := token.NewFileSet()
	var bare []string
	for _, path := range goFilesIn(t, ".") {
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == flushingExit {
				continue
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !callsOSExit(call) {
					return true
				}
				bare = append(bare, relativeToEnvoy(fileSet.Position(call.Pos()).String()))
				return true
			})
		}
	}
	if len(bare) > 0 {
		t.Fatalf("os.Exit is called outside %s() at %s: the process ends there with the reason that step has just logged still buffered, so its failure reaches whoever reads the log with no cause at all. Log and end through fatal(), which writes the lines first.",
			flushingExit, strings.Join(bare, ", "))
	}
}

// callsOSExit reports whether call is os.Exit(...), as every file of this package names it: the
// selector Exit on the os import.
func callsOSExit(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Exit" {
		return false
	}
	imported, ok := selector.X.(*ast.Ident)
	return ok && imported.Name == "os"
}
