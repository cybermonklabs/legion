// Command wholeuse lists every production expression outside package redact whose static type is
// x/oauth2's RetrieveError or *RetrieveError, and how each is used: a field compared, a field or
// the value handed to package redact, the value bound to a local whose own uses are listed, an
// errors.As target, or the value used whole - formatted, serialized, method-called, passed to
// another function, stored or returned. A RetrieveError carries another party's words in
// ErrorCode, ErrorDescription, ErrorURI and Body, so a whole use, and a read of one of those
// fields that is neither compared nor handed to redact, can print them; each is a failure, and the
// command exits 1 naming it. A struct field every write of which is a value redact.NewRetrieveError
// made holds only the rule's reading of the words, so a whole use of a read of that field is
// listed as such and passes.
//
// It reads types through golang.org/x/tools/go/packages, so it sees a RetrieveError however it is
// spelled; it does not follow a value through an interface (an error the chain holds), which no
// static type names.
//
// Usage, from this directory: GOWORK=off go run . -dir ../../../.. ./...
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"wholeuse/load"
)

const (
	oauth2Path = "golang.org/x/oauth2"
	redactPath = "github.com/sjawhar/envoy/internal/redact"
)

// textFields are RetrieveError's fields that hold another party's words.
var textFields = map[string]bool{"ErrorCode": true, "ErrorDescription": true, "ErrorURI": true, "Body": true}

type use struct {
	pos      token.Position
	class    string
	fails    bool
	expr     string
	function string
}

// trustNewRetrieveError is the -trust-new-retrieve-error flag.
var trustNewRetrieveError bool

func main() {
	dir := flag.String("dir", ".", "the module directory to load from")
	flag.BoolVar(&trustNewRetrieveError, "trust-new-retrieve-error", true,
		"pass a value redact.NewRetrieveError made, which holds the rule's reading of the words; false lists it as the whole use it is where NewRetrieveError holds the words as they came")
	targets := flag.String("targets", load.Host, "comma-separated GOOS/GOARCH pairs the module is loaded for")
	tagSets := flag.String("tags", "", "semicolon-separated build tag lists the module is loaded with, each with every target; an empty list is no tags")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	pkgs := load.Modules("wholeuse", *dir, *targets, *tagSets, packages.NeedName|packages.NeedFiles|packages.NeedSyntax|packages.NeedTypes|packages.NeedTypesInfo, patterns...).Packages
	if len(pkgs) == 0 {
		fmt.Fprintln(os.Stderr, "wholeuse: UNKNOWN: the patterns matched no package")
		os.Exit(2)
	}
	madeByRedact := map[*types.Var]bool{}
	if trustNewRetrieveError {
		madeByRedact = redactMadeFields(pkgs)
	}
	var uses []use
	files := 0
	walked := map[string]bool{}
	for _, pkg := range pkgs {
		if pkg.PkgPath == redactPath || strings.HasPrefix(pkg.PkgPath, redactPath+"/") {
			continue
		}
		for _, file := range pkg.Syntax {
			name := pkg.Fset.Position(file.Pos()).Filename
			if strings.HasSuffix(name, "_test.go") || walked[name] {
				continue
			}
			walked[name] = true
			files++
			uses = append(uses, fileUses(pkg, file, madeByRedact)...)
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].pos.Filename != uses[j].pos.Filename {
			return uses[i].pos.Filename < uses[j].pos.Filename
		}
		return uses[i].pos.Offset < uses[j].pos.Offset
	})
	failed := 0
	root, _ := filepath.Abs(*dir)
	for _, u := range uses {
		verdict := "ok"
		if u.fails {
			verdict, failed = "FAIL", failed+1
		}
		rel, err := filepath.Rel(root, u.pos.Filename)
		if err != nil {
			rel = u.pos.Filename
		}
		fmt.Printf("%s\t%s:%d:%d\t%s\t%s\t%s\n", verdict, rel, u.pos.Line, u.pos.Column, u.function, u.class, u.expr)
	}
	fmt.Printf("SUMMARY\tpackages=%d\tproduction files=%d\tuses=%d\tfailures=%d\n", len(pkgs), files, len(uses), failed)
	if files == 0 {
		fmt.Println("UNKNOWN: no production file was read")
		os.Exit(2)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// retrieveError reports whether t is oauth2.RetrieveError or a pointer to it.
func retrieveError(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == oauth2Path && n.Obj().Name() == "RetrieveError"
}

// redactCall reports whether call calls a function or method of package redact, naming it.
func redactCall(info *types.Info, call *ast.CallExpr) (string, bool) {
	fn := calleeFunc(info, call)
	if fn == nil || fn.Pkg() == nil {
		return "", false
	}
	return fn.Pkg().Path() + "." + fn.Name(), fn.Pkg().Path() == redactPath
}

// newRetrieveErrorCall reports whether call calls redact.NewRetrieveError.
func newRetrieveErrorCall(info *types.Info, call *ast.CallExpr) bool {
	name, inRedact := redactCall(info, call)
	return inRedact && strings.HasSuffix(name, ".NewRetrieveError")
}

// calleeFunc is the function call calls, past an explicit instantiation
// (redact.OAuth2[*oauth2.Token](...)), or nil.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	callee := ast.Unparen(call.Fun)
	switch index := callee.(type) {
	case *ast.IndexExpr:
		callee = index.X
	case *ast.IndexListExpr:
		callee = index.X
	}
	var id *ast.Ident
	switch f := callee.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	}
	if id == nil {
		return nil
	}
	fn, _ := info.Uses[id].(*types.Func)
	return fn
}

// redactMadeFields is each struct field of a RetrieveError type every write of which, in a
// composite literal or an assignment, is a call of redact.NewRetrieveError.
func redactMadeFields(pkgs []*packages.Package) map[*types.Var]bool {
	made := map[*types.Var]bool{}
	record := func(info *types.Info, field *types.Var, value ast.Expr) {
		call, ok := ast.Unparen(value).(*ast.CallExpr)
		safe := ok && newRetrieveErrorCall(info, call)
		earlier, seen := made[field]
		made[field] = safe && (!seen || earlier)
	}
	for _, pkg := range pkgs {
		info := pkg.TypesInfo
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok {
						if field, ok := info.Uses[key].(*types.Var); ok && field.IsField() && retrieveError(field.Type()) {
							record(info, field, n.Value)
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || i >= len(n.Rhs) {
							continue
						}
						if field, ok := info.Uses[sel.Sel].(*types.Var); ok && field.IsField() && retrieveError(field.Type()) {
							record(info, field, n.Rhs[i])
						}
					}
				}
				return true
			})
		}
	}
	return made
}

func fileUses(pkg *packages.Package, file *ast.File, madeByRedact map[*types.Var]bool) []use {
	info := pkg.TypesInfo
	var uses []use
	var stack []ast.Node
	function := "(package level)"
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if fd, ok := n.(*ast.FuncDecl); ok {
			function = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				function = types.ExprString(fd.Recv.List[0].Type) + "." + function
			}
		}
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		tv, ok := info.Types[e]
		if !ok || tv.IsType() || !retrieveError(tv.Type) {
			return true
		}
		parent := parentOf(stack)
		// A parenthesis or a dereference of the value is classified where it is used.
		switch p := parent.(type) {
		case *ast.ParenExpr:
			return true
		case *ast.StarExpr:
			if pt, ok := info.Types[p]; ok && retrieveError(pt.Type) {
				return true
			}
		}
		class, fails := classify(info, stack, e, madeByRedact)
		uses = append(uses, use{pos: pkg.Fset.Position(e.Pos()), class: class, fails: fails, expr: types.ExprString(e), function: function})
		return true
	})
	return uses
}

// parentOf is the node above the last on stack.
func parentOf(stack []ast.Node) ast.Node {
	if len(stack) < 2 {
		return nil
	}
	return stack[len(stack)-2]
}

// usedAt is the node that uses the expression at the top of stack, past parentheses.
func usedAt(stack []ast.Node) (ast.Node, []ast.Node) {
	i := len(stack) - 2
	for i >= 0 {
		if _, ok := stack[i].(*ast.ParenExpr); !ok {
			break
		}
		i--
	}
	if i < 0 {
		return nil, nil
	}
	return stack[i], stack[:i+1]
}

func classify(info *types.Info, stack []ast.Node, e ast.Expr, madeByRedact map[*types.Var]bool) (string, bool) {
	parent, above := usedAt(stack)
	// A read of a struct field that only redact.NewRetrieveError writes holds the rule's reading.
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if field, ok := info.Uses[sel.Sel].(*types.Var); ok && madeByRedact[field] {
			return "whole value redact.NewRetrieveError made (" + describe(info, parent) + ")", false
		}
	}
	if call, ok := e.(*ast.CallExpr); ok && newRetrieveErrorCall(info, call) && trustNewRetrieveError {
		return "made by redact.NewRetrieveError", false
	}
	switch p := parent.(type) {
	case *ast.SelectorExpr:
		sel := info.Selections[p]
		if sel == nil {
			return "selector " + p.Sel.Name, true
		}
		if sel.Kind() != types.FieldVal {
			return "method-called whole: " + p.Sel.Name, true
		}
		if !textFields[p.Sel.Name] {
			return "field read: " + p.Sel.Name, false
		}
		user, _ := usedAt(above)
		switch u := user.(type) {
		case *ast.BinaryExpr:
			if u.Op == token.EQL || u.Op == token.NEQ {
				return "field comparison: " + p.Sel.Name, false
			}
		case *ast.SwitchStmt:
			if u.Tag == p {
				return "field comparison (switch): " + p.Sel.Name, false
			}
		case *ast.CallExpr:
			if name, inRedact := redactCall(info, u); inRedact {
				return "field " + p.Sel.Name + " handed to " + name, false
			}
		}
		return "field " + p.Sel.Name + " used outside a comparison or redact (" + describe(info, user) + ")", true
	case *ast.UnaryExpr:
		if p.Op == token.AND {
			if call, ok := parentOf(above).(*ast.CallExpr); ok {
				if fn := calleeFunc(info, call); fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == "errors" && fn.Name() == "As" {
					return "errors.As target", false
				}
			}
			return "address taken", true
		}
	case *ast.BinaryExpr:
		if p.Op == token.EQL || p.Op == token.NEQ {
			return "compared whole", false
		}
	case *ast.CallExpr:
		if name, inRedact := redactCall(info, p); inRedact {
			return "handed to " + name, false
		}
	case *ast.AssignStmt, *ast.ValueSpec:
		if boundToLocal(info, p, e) {
			return "bound to a local (its uses are listed)", false
		}
	}
	return describe(info, parent), true
}

// boundToLocal reports whether e is assigned to a local variable: alone, or as the value of a
// two-value type assertion (x, ok := e.(T)).
func boundToLocal(info *types.Info, n ast.Node, e ast.Expr) bool {
	var lhs []ast.Expr
	var rhs []ast.Expr
	switch n := n.(type) {
	case *ast.AssignStmt:
		lhs, rhs = n.Lhs, n.Rhs
	case *ast.ValueSpec:
		for _, name := range n.Names {
			lhs = append(lhs, name)
		}
		rhs = n.Values
	}
	for i, r := range rhs {
		if r != e || i >= len(lhs) {
			continue
		}
		id, ok := lhs[i].(*ast.Ident)
		if !ok {
			return false
		}
		v, ok := info.ObjectOf(id).(*types.Var)
		return ok && !v.IsField() && v.Parent() != v.Pkg().Scope()
	}
	return false
}

// describe names how parent uses a value: formatted, serialized, passed, stored or returned.
func describe(info *types.Info, parent ast.Node) string {
	switch p := parent.(type) {
	case *ast.CallExpr:
		fn := calleeFunc(info, p)
		if fn == nil || fn.Pkg() == nil {
			return "passed whole to " + types.ExprString(p.Fun)
		}
		path := fn.Pkg().Path()
		switch {
		case path == "fmt" || path == "log" || path == "log/slog" || strings.HasPrefix(path, "log/"):
			return "formatted whole by " + path + "." + fn.Name()
		case strings.HasPrefix(path, "encoding/"):
			return "serialized whole by " + path + "." + fn.Name()
		}
		return "passed whole to " + path + "." + fn.Name()
	case *ast.ReturnStmt:
		return "returned whole"
	case *ast.KeyValueExpr, *ast.CompositeLit:
		return "stored whole in a composite literal"
	case *ast.AssignStmt, *ast.ValueSpec:
		return "stored whole"
	case *ast.SendStmt:
		return "sent whole on a channel"
	case nil:
		return "unknown use"
	}
	return fmt.Sprintf("used whole by %T", parent)
}
