package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

// airlockDir is the airlock's proxy port, whose refusals its own recorder writes (refuse in its
// bound.go, record in its record.go), as answer.Middleware records the API port's.
const airlockDir = envoyDir + "/internal/broker/airlockproxy"

// refusalDirs is the packages checkRefusalWrites reads, in the order it lists their problems.
var refusalDirs = []string{apiDir, approvalDir, airlockDir}

// writerKey names a function of one package as refgen can name it with no type checker: the
// type its receiver is declared on ("" for a function) and its name. The name alone is not
// enough: airlockproxy declares Write, Read and Close on more than one type, which is also why
// source.funcs, keyed by the name, refuses that package.
type writerKey struct{ recv, name string }

func (k writerKey) String() string {
	if k.recv == "" {
		return k.name
	}
	return k.recv + "." + k.name
}

// registeredWriters is, for each package whose refusals a recorder records, each function that
// writes one of them with the code the refusal's line and its count name: in the API package
// writeError and writePageRefusal, whose JSON bodies carry the code; on the approval page
// writeMessage, whose callers note theirs (answer.NoteCode); on the airlock's proxy port
// Server.refuse, which writes the port's refusal line itself, and Server.refuseCredential, its
// 407. A status from 400 to 499 reaches the wire only as the status argument of one of these, and
// what one writes in its own body is its own. The API package has a third registered writer that
// is no function: the Status field of an oauth.Refusal composite, which oauth.WriteRefusal writes
// with its code in a package this rule does not read; a composite literal is no write, so the rule
// has nothing of it to check.
//
// A table is needed for the reason pageAnswerers is one: refgen parses with go/ast and runs no
// type checker, so it names a writer by its package, its receiver and its name.
var registeredWriters = map[string][]writerKey{
	apiDir:      {{"", "writeError"}, {"", "writePageRefusal"}},
	approvalDir: {{"", "writeMessage"}},
	airlockDir:  {{"Server", "refuse"}, {"Server", "refuseCredential"}},
}

// forwardingWrites is each write checkRefusalWrites leaves out, because the status it writes is
// not the broker's own: airlockproxy's statusWriter.WriteHeader, which only httputil.ReverseProxy
// calls, with the status the upstream answered; and answerWriter.WriteHeader, the airlock
// recorder's wrapper, which records the status its handler wrote and forwards it as it came.
var forwardingWrites = map[string][]writerKey{
	airlockDir: {{"statusWriter", "WriteHeader"}, {"answerWriter", "WriteHeader"}},
}

// checkRefusalWrites refuses a refusal its port's recorder would record without the code a
// registered writer names: in each package of refusalDirs, a raw write (w.WriteHeader(status),
// http.Error(w, message, status) or http.Redirect(w, r, url, status)) outside the body of every
// registeredWriters and forwardingWrites function, whose status is from 400 to 499 or is one
// refgen cannot read as a constant. A status that is the enclosing function's own parameter is
// followed to each call of that function, and one a local variable holds to each value it is
// assigned; a status only returned or compared writes nothing, and is not read. Each problem
// names the file and line of the status, and of the write when the status is on another line.
func checkRefusalWrites(root string) error {
	var problems []string
	for _, dir := range refusalDirs {
		src, err := parseDir(root, dir)
		if err != nil {
			return err
		}
		problems = append(problems, readWrites(src).check()...)
	}
	return refusedAt(problems)
}

// writes is one package read for checkRefusalWrites: its functions, every call in it and every
// value each function's local variables are assigned, each with the function it is in.
type writes struct {
	src   *source
	decls map[writerKey]*ast.FuncDecl
	calls []callSite
	// assigned is every value a name is assigned in each function, its function literals'
	// assignments included, since a literal assigns the variables it closes over.
	assigned map[*ast.FuncDecl]map[string][]assignment
	// identUses and selectorUses are each reference to a name that is no call of it: a function
	// taken as a value, or a method value.
	identUses, selectorUses map[string][]ast.Node
	skipped                 map[*ast.FuncDecl]bool // the bodies the rule does not read
	following               map[local]bool         // the names being read, so a cycle ends
	seen                    map[string]bool
	problems                []string
}

// scope is where a node sits: the function declaring it (nil at package level) and the function
// literals around it, outermost first.
type scope struct {
	fn   *ast.FuncDecl
	lits []*ast.FuncLit
}

type callSite struct {
	call *ast.CallExpr
	scope
}

// assignment is one value a function's variable is assigned; value is nil when refgen cannot pair
// one with the name (a, b := f(), n += 1, a range variable).
type assignment struct {
	name  *ast.Ident
	value ast.Expr
	scope
}

// local is a name as one function reads it.
type local struct {
	fn   *ast.FuncDecl
	name string
}

// write is one raw write the rule reads: its callee as written, and where it is.
type write struct{ what, at string }

// readWrites walks every declaration of src itself, rather than through source.funcs, which
// refuses a package declaring one method name on two types.
func readWrites(src *source) *writes {
	w := &writes{
		src:          src,
		decls:        map[writerKey]*ast.FuncDecl{},
		assigned:     map[*ast.FuncDecl]map[string][]assignment{},
		identUses:    map[string][]ast.Node{},
		selectorUses: map[string][]ast.Node{},
		skipped:      map[*ast.FuncDecl]bool{},
		following:    map[local]bool{},
		seen:         map[string]bool{},
	}
	for _, f := range src.files {
		// callee and declared are the nodes no reference is read from: each call's callee, and
		// each name being declared, and each selector's field or method name.
		callee := map[ast.Node]bool{}
		declared := map[*ast.Ident]bool{}
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.FuncDecl:
				declared[x.Name] = true
				if x.Body != nil {
					w.decls[keyOf(x)] = x
				}
			case *ast.Field:
				for _, name := range x.Names {
					declared[name] = true
				}
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok {
					declared[key] = true
				}
			case *ast.LabeledStmt:
				declared[x.Label] = true
			case *ast.BranchStmt:
				if x.Label != nil {
					declared[x.Label] = true
				}
			case *ast.ValueSpec:
				for i, name := range x.Names {
					declared[name] = true
					switch {
					case len(x.Values) == len(x.Names):
						w.assign(stack, name, x.Values[i])
					case len(x.Values) > 0 || x.Type == nil:
						w.assign(stack, name, nil)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					name, ok := lhs.(*ast.Ident)
					if !ok {
						continue
					}
					if x.Tok == token.DEFINE {
						declared[name] = true
					}
					if (x.Tok == token.DEFINE || x.Tok == token.ASSIGN) && len(x.Lhs) == len(x.Rhs) {
						w.assign(stack, name, x.Rhs[i])
					} else {
						w.assign(stack, name, nil)
					}
				}
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if name, ok := e.(*ast.Ident); ok {
						if x.Tok == token.DEFINE {
							declared[name] = true
						}
						w.assign(stack, name, nil)
					}
				}
			case *ast.IncDecStmt:
				if name, ok := x.X.(*ast.Ident); ok {
					w.assign(stack, name, nil)
				}
			case *ast.CallExpr:
				callee[x.Fun] = true
				w.calls = append(w.calls, callSite{call: x, scope: scopeOf(stack)})
			case *ast.SelectorExpr:
				declared[x.Sel] = true
				if !callee[x] {
					w.selectorUses[x.Sel.Name] = append(w.selectorUses[x.Sel.Name], x)
				}
			case *ast.Ident:
				if !declared[x] && !callee[x] {
					w.identUses[x.Name] = append(w.identUses[x.Name], x)
				}
			}
			return true
		})
	}
	return w
}

// assign records that name, at the innermost node of stack, is assigned value there.
func (w *writes) assign(stack []ast.Node, name *ast.Ident, value ast.Expr) {
	sc := scopeOf(stack)
	if sc.fn == nil {
		return // a package-level variable, which constant does not read
	}
	if w.assigned[sc.fn] == nil {
		w.assigned[sc.fn] = map[string][]assignment{}
	}
	w.assigned[sc.fn][name.Name] = append(w.assigned[sc.fn][name.Name], assignment{name: name, value: value, scope: sc})
}

func scopeOf(stack []ast.Node) scope {
	var sc scope
	for _, n := range stack {
		switch x := n.(type) {
		case *ast.FuncDecl:
			sc.fn = x
		case *ast.FuncLit:
			sc.lits = append(sc.lits, x)
		}
	}
	return sc
}

// keyOf is fn's receiver type and name.
func keyOf(fn *ast.FuncDecl) writerKey {
	k := writerKey{name: fn.Name.Name}
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return k
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	if id, ok := t.(*ast.Ident); ok {
		k.recv = id.Name
	}
	return k
}

// check lists every problem of the package's raw writes.
func (w *writes) check() []string {
	dir := w.src.dir
	for _, table := range []struct {
		name string
		keys []writerKey
	}{{"registeredWriters", registeredWriters[dir]}, {"forwardingWrites", forwardingWrites[dir]}} {
		for _, k := range table.keys {
			fn := w.decls[k]
			if fn == nil {
				w.refuse(fmt.Sprintf("%s declares no %s, which %s names", dir, k, table.name))
				continue
			}
			w.skipped[fn] = true
		}
	}
	for _, c := range w.calls {
		status, ok := rawWrite(c.call)
		if !ok || w.skipped[c.fn] {
			continue
		}
		w.status(status, c.scope, write{what: w.src.text(c.call.Fun), at: w.src.at(c.call)})
	}
	return w.problems
}

// rawWrite is the status a raw write writes: w.WriteHeader(status), http.Error(w, message,
// status) or http.Redirect(w, r, url, status).
func rawWrite(call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if sel.Sel.Name == "WriteHeader" && len(call.Args) == 1 {
		return call.Args[0], true
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
		return nil, false
	}
	switch {
	case sel.Sel.Name == "Error" && len(call.Args) == 3:
		return call.Args[2], true
	case sel.Sel.Name == "Redirect" && len(call.Args) == 4:
		return call.Args[3], true
	}
	return nil, false
}

// status reads e, a status that sc holds and that reaches the write wr: a literal or an
// http.Status constant is checked, a name is read further, and anything else cannot be read.
func (w *writes) status(e ast.Expr, sc scope, wr write) {
	switch x := e.(type) {
	case *ast.ParenExpr:
		w.status(x.X, sc, wr)
		return
	case *ast.BasicLit:
		if code, ok := intLit(x); ok {
			w.code(code, x, wr)
			return
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" {
			if code, ok := httpStatus[x.Sel.Name]; ok {
				w.code(code, x, wr)
				return
			}
		}
	case *ast.Ident:
		w.name(x, sc, wr)
		return
	}
	w.unreadable(e, wr, "")
}

// name reads a status a name holds: a parameter of the function declaring it through each call
// of that function, a local variable through each value it is assigned, and a package-level
// constant through its value. A parameter of a function literal cannot be read, since nothing
// names the literal's calls.
func (w *writes) name(id *ast.Ident, sc scope, wr write) {
	for i := len(sc.lits) - 1; i >= 0; i-- {
		if _, _, ok := paramAt(sc.lits[i].Type.Params, id.Name); ok {
			w.unreadable(id, wr, "it is a parameter of a function literal, whose calls refgen cannot read")
			return
		}
	}
	key := local{sc.fn, id.Name}
	if w.following[key] {
		return // already being read, further up this same chain
	}
	w.following[key] = true
	defer delete(w.following, key)
	if sc.fn != nil {
		i, field, isParam := paramAt(sc.fn.Type.Params, id.Name)
		if isParam {
			w.callers(sc.fn, i, field, id, wr)
		}
		for _, a := range w.assigned[sc.fn][id.Name] {
			if a.value == nil {
				w.unreadable(a.name, wr, "refgen cannot tell which value it is assigned here")
				continue
			}
			w.status(a.value, a.scope, wr)
		}
		if isParam || len(w.assigned[sc.fn][id.Name]) > 0 {
			return
		}
	}
	if value, ok := w.constant(id.Name); ok {
		w.status(value, scope{}, wr)
		return
	}
	w.unreadable(id, wr, "")
}

// callers reads the status fn's parameter i (id where wr writes it, declared in field) holds
// from each call of fn in the package, where each call is. A call inside a registered writer is
// that writer's own. A function whose calls refgen cannot all see - an exported one, one taken
// as a value - cannot be read.
func (w *writes) callers(fn *ast.FuncDecl, i int, field *ast.Field, id *ast.Ident, wr write) {
	key := keyOf(fn)
	if _, variadic := field.Type.(*ast.Ellipsis); variadic {
		w.unreadable(id, wr, fmt.Sprintf("it is the variadic parameter of %s", key))
		return
	}
	if ast.IsExported(key.name) {
		w.unreadable(id, wr, fmt.Sprintf("it is a parameter of %s, which is exported, so not every call of it is in this package", key))
		return
	}
	uses := w.identUses[key.name]
	if fn.Recv != nil {
		uses = w.selectorUses[key.name]
	}
	if len(uses) > 0 {
		w.unreadable(id, wr, fmt.Sprintf("it is a parameter of %s, which %s takes as a value, so not every call of it can be read", key, w.src.at(uses[0])))
		return
	}
	for _, c := range w.calls {
		if !calls(c.call, fn) || w.skipped[c.fn] {
			continue
		}
		if i >= len(c.call.Args) {
			w.unreadable(c.call, wr, fmt.Sprintf("this call of %s passes nothing refgen can read for %s", key, id.Name))
			continue
		}
		w.status(c.call.Args[i], c.scope, wr)
	}
}

// calls reports whether call may call fn: by its name, a function's as an identifier and a
// method's as a selector. With no type checker a method's name is matched on any receiver, so a
// call of another type's method of that name is read as well.
func calls(call *ast.CallExpr, fn *ast.FuncDecl) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return fn.Recv == nil && f.Name == fn.Name.Name
	case *ast.SelectorExpr:
		return fn.Recv != nil && f.Sel.Name == fn.Name.Name
	}
	return false
}

// paramAt is the position of the parameter name in params, and the field declaring it.
func paramAt(params *ast.FieldList, name string) (int, *ast.Field, bool) {
	if params == nil {
		return 0, nil, false
	}
	i := 0
	for _, field := range params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i, field, true
			}
			i++
		}
	}
	return 0, nil, false
}

// constant is the value a package-level constant is declared with.
func (w *writes) constant(name string) (ast.Expr, bool) {
	for spec := range w.src.specs(token.CONST) {
		vs := spec.(*ast.ValueSpec)
		for i, n := range vs.Names {
			if n.Name == name && i < len(vs.Values) {
				return vs.Values[i], true
			}
		}
	}
	return nil, false
}

// code checks one status e is, which reaches the write wr.
func (w *writes) code(code int, e ast.Expr, wr write) {
	if code < 400 || code > 499 {
		return
	}
	at, writers := w.src.at(e), oneOf(registeredWriters[w.src.dir])
	if at == wr.at {
		w.refuse(fmt.Sprintf("%s: %s writes %d outside a registered writer; a refusal is written through %s", at, wr.what, code, writers))
		return
	}
	w.refuse(fmt.Sprintf("%s: %s is %d, which %s at %s writes outside a registered writer; a refusal is written through %s", at, w.src.text(e), code, wr.what, wr.at, writers))
}

// unreadable records a status e that refgen cannot read as a constant, which reaches the write
// wr; why, when it is not empty, says what stops it.
func (w *writes) unreadable(e ast.Expr, wr write, why string) {
	if why != "" {
		why = ": " + why
	}
	at := w.src.at(e)
	if at == wr.at {
		w.refuse(fmt.Sprintf("%s: %s writes %s, a status refgen cannot read as a constant%s", at, wr.what, w.src.text(e), why))
		return
	}
	w.refuse(fmt.Sprintf("%s: %s, which %s at %s writes, is a status refgen cannot read as a constant%s", at, w.src.text(e), wr.what, wr.at, why))
}

func (w *writes) refuse(problem string) {
	if !w.seen[problem] {
		w.seen[problem] = true
		w.problems = append(w.problems, problem)
	}
}

// oneOf is "a", "a or b", "a, b or c".
func oneOf(keys []writerKey) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.String()
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
