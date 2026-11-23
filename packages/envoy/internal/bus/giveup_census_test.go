package bus

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// givenUpWrites are the bucket writes of KeyValue a caller can leave running: a write's call is
// one of these.
var givenUpWrites = map[string]bool{
	"Put": true, "PutString": true, "Create": true, "Update": true, "Delete": true,
	"DeleteFenced": true, "DeleteAt": true, "DeleteAtRead": true, "Purge": true,
}

// Every bucket write that a caller can give up on passes that caller's context to the handle
// (Within), so none of it is sent after the caller gave up. bus.Bounded and cistore's kvCall leave
// their call running when their context ends, so a write inside one sent through a handle given no
// context runs on to its own wait, sent again for the whole of it, and can land after the caller's
// next write: a session's registration after its delete, or a spent-proof record after the 503
// that gave the request's proof back (Tester4926's finding on #4926).
//
// The row reads every non-test Go file of the envoy module, finds each function literal passed to
// bus.Bounded, Bounded or kvCall, and names each call of a bucket write inside one whose receiver
// is neither a Within call nor a name the enclosing function assigned from one. It fails when it
// read no such literal or no bound write, since a census that enumerated nothing has passed
// nothing. A handle the literal passes to a function that writes through it (store's
// createInterest) is not followed: the census reads only the writes called in the literal itself.
// Control: any one of the sites written through a handle given no context, for example
// `s.kv.Create(key, nil)` in cmd/listener's CheckAndSetDPoPProofUsed (Tester4926's qJ), or
// `kv.Put(key, buf)` in cistore's requestVerify; the row fails naming that file and line.
func TestEveryGivenUpBucketWritePassesItsCallersContext(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var files, literals, bound int
	var unbound []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			within := withinAssigned(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				giveUp := givenUpCall(call)
				if giveUp == "" {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.FuncLit)
					if !ok {
						continue
					}
					literals++
					ast.Inspect(lit.Body, func(m ast.Node) bool {
						write, ok := m.(*ast.CallExpr)
						if !ok {
							return true
						}
						sel, ok := write.Fun.(*ast.SelectorExpr)
						if !ok || !givenUpWrites[sel.Sel.Name] {
							return true
						}
						if isWithinCall(sel.X) || isWithinName(sel.X, within) {
							bound++
							return true
						}
						unbound = append(unbound, fset.Position(write.Pos()).String()+": "+exprText(fset, sel.X)+"."+sel.Sel.Name+" inside "+giveUp+", through a handle given no context")
						return true
					})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files under %s: %v", root, err)
	}
	t.Logf("%d Go files, %d function literals passed to bus.Bounded, Bounded or kvCall, %d bucket writes in them through a handle given the caller's context, %d through one given none", files, literals, bound, len(unbound))
	if literals == 0 || bound == 0 {
		t.Fatalf("UNKNOWN: the census read %d function literals and %d bound writes under %s; it enumerated nothing, so it shows nothing", literals, bound, root)
	}
	for _, site := range unbound {
		t.Errorf("%s: it can land after its caller gave up", site)
	}
}

// givenUpCall names call when it is bus.Bounded, Bounded or kvCall, each of which leaves its
// function running when its context ends, and is "" otherwise.
func givenUpCall(call *ast.CallExpr) string {
	fun := call.Fun
	if index, ok := fun.(*ast.IndexExpr); ok {
		fun = index.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		if f.Name == "Bounded" || f.Name == "kvCall" {
			return f.Name
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok && x.Name == "bus" && f.Sel.Name == "Bounded" {
			return "bus.Bounded"
		}
	}
	return ""
}

// isWithinCall reports whether x is a call of a method named Within.
func isWithinCall(x ast.Expr) bool {
	call, ok := x.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Within"
}

// withinAssigned is the names body assigns from a call of a method named Within.
func withinAssigned(body *ast.BlockStmt) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != len(assign.Rhs) {
			return true
		}
		for i, rhs := range assign.Rhs {
			if ident, ok := assign.Lhs[i].(*ast.Ident); ok && isWithinCall(rhs) {
				names[ident.Name] = true
			}
		}
		return true
	})
	return names
}

// isWithinName reports whether x is a name its function assigned from a Within call.
func isWithinName(x ast.Expr, within map[string]bool) bool {
	ident, ok := x.(*ast.Ident)
	return ok && within[ident.Name]
}

// exprText is x as source.
func exprText(fset *token.FileSet, x ast.Expr) string {
	var text strings.Builder
	if err := printer.Fprint(&text, fset, x); err != nil {
		return "?"
	}
	return text.String()
}
