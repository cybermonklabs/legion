//go:build ignore

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
)

// analysis is the search over one module's files: the opener forms, the wrappers found so far and
// the calls listed.
type analysis struct {
	m     *module
	forms map[string]form
	// wrappers holds each wrapper with the indices of its parameters that reach a connection
	// argument of an opener.
	wrappers map[*types.Func]map[int]bool
	// decls holds the declaration of each function of the module, by its object.
	decls  map[*types.Func]*ast.FuncDecl
	calls  []*call
	rounds int
}

// call is one listed call: of a form, of a wrapper, or a form or wrapper named as a value.
type call struct {
	at       token.Pos
	function string
	// in is the function whose body holds the call; nil in a package variable's declaration.
	in       *types.Func
	callee   string
	wrapped  *types.Func
	argument string
	kind     string // "end", "wrapper", "value" or "uncalled"
	verdict  verdict
}

// verdict is what a judge found of an argument: judged (with why), or a value of the parameters
// it names (a wrapper's), or unjudged (with why).
type verdict struct {
	judged bool
	params map[int]bool
	why    string
}

func newAnalysis(m *module) *analysis {
	a := &analysis{m: m, forms: map[string]form{}, wrappers: map[*types.Func]map[int]bool{}, decls: map[*types.Func]*ast.FuncDecl{}}
	for _, f := range m.files {
		for _, decl := range f.Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && d.Body != nil {
				if fn, ok := m.info.Defs[d.Name].(*types.Func); ok {
					a.decls[fn] = d
				}
			}
		}
	}
	a.readForms()
	return a
}

// answer is the verdict on what fn, a function of the module, answers first: judged when every
// return statement of its body (not those of a function literal in it) answers a value judged
// without its parameters, so that no call of it can hand it a setting; ok is false otherwise.
func (a *analysis) answer(fn *types.Func, depth int) (verdict, bool) {
	decl := a.decls[fn]
	if decl == nil || depth > 4 {
		return verdict{}, false
	}
	params := map[types.Object]int{}
	index := 0
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			index++
		}
		for _, n := range field.Names {
			params[a.m.info.Defs[n]] = index
			index++
		}
	}
	var whys []string
	ok := true
	returns := 0
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			returns++
			if len(x.Results) == 0 {
				ok = false
				return false
			}
			v := a.judge(decl, params, x.Results[0], x.Pos(), depth+1)
			if !v.judged || len(v.params) > 0 {
				ok = false
			}
			whys = append(whys, v.why)
		}
		return true
	})
	if !ok || returns == 0 {
		return verdict{}, false
	}
	return verdict{judged: true, why: fmt.Sprintf("the answer of %s, whose every return is judged: %s", fn.Name(), strings.Join(whys, "; "))}, true
}

// run lists every call, finding wrappers again until a round finds none that is new.
func (a *analysis) run() {
	for {
		a.rounds++
		a.calls = a.calls[:0]
		grew := false
		for _, f := range a.m.files {
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body != nil && a.walk(d, d.Body) {
						grew = true
					}
				case *ast.GenDecl:
					// A package variable's initializer is read as a function of its own with no
					// parameters.
					if a.walk(nil, d) {
						grew = true
					}
				}
			}
		}
		if !grew {
			break
		}
	}
	called := map[*types.Func]bool{}
	for _, c := range a.calls {
		if c.wrapped != nil && c.kind != "value" {
			called[c.wrapped] = true
		}
	}
	for _, c := range a.calls {
		if c.kind == "wrapper" && c.in != nil && !called[c.in] {
			c.kind = "uncalled"
			c.verdict = verdict{why: "a wrapper that no file read calls: its callers are tests"}
		}
	}
	sort.SliceStable(a.calls, func(i, j int) bool {
		pi, pj := a.m.fset.Position(a.calls[i].at), a.m.fset.Position(a.calls[j].at)
		if pi.Filename != pj.Filename {
			return pi.Filename < pj.Filename
		}
		return pi.Offset < pj.Offset
	})
}

// walk lists the calls in node, the body of fd (nil for a package variable's declaration), and
// reports whether it found a wrapper parameter that is new.
func (a *analysis) walk(fd *ast.FuncDecl, node ast.Node) bool {
	info := a.m.info
	var self *types.Func
	params := map[types.Object]int{}
	name := "(package variable)"
	if fd != nil {
		self, _ = info.Defs[fd.Name].(*types.Func)
		name = fd.Name.Name
		index := 0
		for _, field := range fd.Type.Params.List {
			if len(field.Names) == 0 {
				index++
			}
			for _, n := range field.Names {
				params[info.Defs[n]] = index
				index++
			}
		}
	}
	grew := false
	calledFun := map[ast.Expr]bool{}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			fun := ast.Unparen(x.Fun)
			calledFun[fun] = true
			if index, ok := fun.(*ast.IndexExpr); ok {
				calledFun[ast.Unparen(index.X)] = true
			}
			fn := calleeOf(x, info)
			if fn == nil {
				return true
			}
			callee := funcName(fn)
			var positions []int
			if f, ok := a.forms[callee]; ok {
				positions = []int{f.Argument}
			} else if indices, ok := a.wrappers[fn]; ok {
				for i := range indices {
					positions = append(positions, i)
				}
				sort.Ints(positions)
			} else {
				return true
			}
			if fd != nil && a.inFormatter(fd) {
				// The formatter's own parse: pgsetting is the one reading.
				a.calls = append(a.calls, &call{at: x.Pos(), function: name, in: self, callee: callee, argument: a.args(x, positions), kind: "end",
					verdict: verdict{judged: true, why: "the formatter's own parse (pgsetting)"}})
				return true
			}
			var v verdict
			for i, at := range positions {
				if at >= len(x.Args) {
					continue
				}
				one := a.judge(fd, params, x.Args[at], x.Pos(), 0)
				if i == 0 {
					v = one
				} else {
					v = combine(v, one)
				}
			}
			kind := "end"
			if len(v.params) > 0 {
				kind = "wrapper"
				if self != nil {
					if a.wrappers[self] == nil {
						a.wrappers[self] = map[int]bool{}
					}
					for i := range v.params {
						if !a.wrappers[self][i] {
							a.wrappers[self][i] = true
							grew = true
						}
					}
				}
			}
			var wrapped *types.Func
			if _, isWrapper := a.wrappers[fn]; isWrapper {
				wrapped = fn
			}
			a.calls = append(a.calls, &call{at: x.Pos(), function: name, in: self, callee: callee, wrapped: wrapped, argument: a.args(x, positions), kind: kind, verdict: v})
		case *ast.SelectorExpr:
			// The selector is the name; its Sel is not read again.
			a.value(name, self, x, x.Sel, calledFun)
			ast.Inspect(x.X, visit)
			return false
		case *ast.Ident:
			a.value(name, self, x, x, calledFun)
		}
		return true
	}
	ast.Inspect(node, visit)
	return grew
}

// value lists expr, which names id, when it names a form or a wrapper and is not called.
func (a *analysis) value(function string, self *types.Func, expr ast.Expr, id *ast.Ident, calledFun map[ast.Expr]bool) {
	if calledFun[expr] {
		return
	}
	fn, ok := a.m.info.Uses[id].(*types.Func)
	if !ok {
		return
	}
	_, isForm := a.forms[funcName(fn)]
	_, isWrapper := a.wrappers[fn]
	if !isForm && !isWrapper {
		return
	}
	var wrapped *types.Func
	if isWrapper {
		wrapped = fn
	}
	a.calls = append(a.calls, &call{at: expr.Pos(), function: function, in: self, callee: funcName(fn), wrapped: wrapped, argument: a.text(expr), kind: "value",
		verdict: verdict{why: "named as a value, so what it is later handed is not read"}})
}

// inFormatter reports whether fd is a function of the formatter itself.
func (a *analysis) inFormatter(fd *ast.FuncDecl) bool {
	fn, ok := a.m.info.Defs[fd.Name].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == pgsetting
}

// judge is the verdict on e, an argument in the connection place of a call at pos in fd (nil for
// a package variable), whose parameters are params.
func (a *analysis) judge(fd *ast.FuncDecl, params map[types.Object]int, e ast.Expr, pos token.Pos, depth int) verdict {
	info := a.m.info
	e = ast.Unparen(e)
	switch x := e.(type) {
	case *ast.StarExpr:
		return a.judge(fd, params, x.X, pos, depth)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return a.judge(fd, params, x.X, pos, depth)
		}
	}
	if depth > 8 {
		return verdict{why: "a value read through more than 8 assignments"}
	}
	if line, how := a.guard(fd, e, pos); line > 0 {
		return verdict{judged: true, why: fmt.Sprintf("guarded by %s, at line %d", how, line)}
	}
	if tv, ok := info.Types[e]; ok && tv.Value != nil {
		return verdict{judged: true, why: "a constant"}
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		return verdict{judged: true, why: "a constant"}
	case *ast.CompositeLit:
		if len(x.Elts) == 0 {
			return verdict{judged: true, why: "a zero value"}
		}
	case *ast.Ident:
		switch obj := info.Uses[x].(type) {
		case *types.Const:
			return verdict{judged: true, why: "a constant"}
		case *types.Nil:
			return verdict{judged: true, why: "nil"}
		case *types.Var:
			if i, ok := params[obj]; ok {
				return verdict{params: map[int]bool{i: true}, why: "a parameter"}
			}
			if fd == nil || obj.Pos() < fd.Pos() || obj.Pos() >= fd.End() {
				return verdict{why: "the package variable " + x.Name}
			}
			values, unknown := a.assigned(fd, obj)
			if unknown != "" {
				return verdict{why: unknown}
			}
			v := verdict{judged: true, why: "a zero value"}
			for i, value := range values {
				one := verdict{judged: true, why: "a zero value"}
				if value != nil {
					one = a.judge(fd, params, value, value.Pos(), depth+1)
				}
				if i == 0 {
					v = one
				} else {
					v = combine(v, one)
				}
			}
			return v
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return combine(a.judge(fd, params, x.X, pos, depth+1), a.judge(fd, params, x.Y, pos, depth+1))
		}
	case *ast.CallExpr:
		if tv, ok := info.Types[x.Fun]; ok && tv.IsType() && len(x.Args) == 1 {
			return a.judge(fd, params, x.Args[0], pos, depth+1)
		}
		fn := calleeOf(x, info)
		if fn == nil {
			return verdict{why: "the answer of " + a.short(x.Fun)}
		}
		callee := funcName(fn)
		switch {
		case formatterParses[callee]:
			return verdict{judged: true, why: "read through " + strings.TrimPrefix(callee, "github.com/sjawhar/envoy/")}
		case a.forms[callee].Callee != "":
			return verdict{judged: true, why: fmt.Sprintf("the configuration the parse at line %d answered, which is listed itself", a.m.fset.Position(x.Pos()).Line)}
		case fn.Name() == "Copy" && configType(recvType(fn)) != "":
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
				return a.judge(fd, params, sel.X, pos, depth+1)
			}
		case fn.Name() == "Config" && openConnection(recvType(fn)):
			return verdict{judged: true, why: "the configuration of a pool or connection already open, whose own opener is listed"}
		}
		if v, ok := a.answer(fn, depth); ok {
			return v
		}
		return verdict{why: "the answer of " + strings.TrimPrefix(callee, a.m.path+"/")}
	case *ast.SelectorExpr:
		return verdict{why: "the field " + a.short(x)}
	}
	return verdict{why: "the value " + a.short(e)}
}

// combine is the verdict on a value built from two: unjudged when either is, a wrapper's when
// either names a parameter, judged otherwise.
func combine(x, y verdict) verdict {
	switch {
	case !x.judged && len(x.params) == 0:
		return x
	case !y.judged && len(y.params) == 0:
		return y
	case len(x.params) > 0 || len(y.params) > 0:
		params := map[int]bool{}
		for i := range x.params {
			params[i] = true
		}
		for i := range y.params {
			params[i] = true
		}
		return verdict{params: params, why: "a parameter"}
	}
	if x.why == y.why {
		return x
	}
	return verdict{judged: true, why: x.why + "; " + y.why}
}

// assigned is every value fd assigns to obj, a variable declared in it (nil for its zero value),
// or why they cannot all be read: its address is taken, or a range statement writes it.
func (a *analysis) assigned(fd *ast.FuncDecl, obj types.Object) ([]ast.Expr, string) {
	info := a.m.info
	var values []ast.Expr
	unknown := ""
	is := func(e ast.Expr) bool {
		id, ok := ast.Unparen(e).(*ast.Ident)
		return ok && (info.Defs[id] == obj || info.Uses[id] == obj)
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if !is(lhs) {
					continue
				}
				switch {
				case x.Tok != token.ASSIGN && x.Tok != token.DEFINE:
					unknown = "a variable an operator assigns"
				case len(x.Rhs) == len(x.Lhs):
					values = append(values, x.Rhs[i])
				default:
					values = append(values, x.Rhs[0])
				}
			}
		case *ast.ValueSpec:
			for i, n := range x.Names {
				if info.Defs[n] != obj {
					continue
				}
				switch {
				case len(x.Values) == 0:
					values = append(values, nil)
				case len(x.Values) == len(x.Names):
					values = append(values, x.Values[i])
				default:
					values = append(values, x.Values[0])
				}
			}
		case *ast.RangeStmt:
			if (x.Key != nil && is(x.Key)) || (x.Value != nil && is(x.Value)) {
				unknown = "a variable a range statement writes"
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND && is(x.X) {
				unknown = "a variable whose address is taken, so another line can write it"
			}
		}
		return true
	})
	if unknown == "" && len(values) == 0 {
		unknown = "a variable no line of its function assigns"
	}
	return values, unknown
}

// guard is the line of a parse of the formatter (pgsetting.ParseConfig or ParsePoolConfig) in fd
// that guards e at pos, and which parse it is; 0 when none does. A parse guards e when its error
// ends the function (refusal), it is a statement of a block that holds pos, before the statement
// that holds pos, in the same function literal as pos, it read the very variable or field e names
// (same), and no line can change that value between the two (unwritten). A goto in fd can jump
// past the parse, so fd then has no guard. pgsetting.Refuse guards nothing (formatterParses).
func (a *analysis) guard(fd *ast.FuncDecl, e ast.Expr, pos token.Pos) (int, string) {
	if fd == nil || hasGoto(fd) {
		return 0, ""
	}
	nodes := enclosing(fd.Body, pos)
	// Only a block after the last function literal that holds pos runs on pos's path each time.
	first := 0
	for i, n := range nodes {
		if _, ok := n.(*ast.FuncLit); ok {
			first = i + 1
		}
	}
	for k := first; k < len(nodes)-1; k++ {
		list := statements(nodes[k])
		if list == nil {
			continue
		}
		holding := -1
		for i, s := range list {
			if s == nodes[k+1] {
				holding = i
			}
		}
		for i := range max(holding, 0) {
			var next ast.Stmt
			if i+1 < holding {
				next = list[i+1]
			}
			c := a.refusal(list[i], next)
			if c == nil || !a.same(c.Args[1], e) || !a.unwritten(fd, e, list[i].End(), pos, nodes[k+1:]) {
				continue
			}
			fn := calleeOf(c, a.m.info)
			return a.m.fset.Position(c.Pos()).Line, "pgsetting." + fn.Name()
		}
	}
	return 0, ""
}

// enclosing is the nodes of root that hold pos, outermost first.
func enclosing(root ast.Node, pos token.Pos) []ast.Node {
	var nodes []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil || pos < n.Pos() || pos >= n.End() {
			return false
		}
		nodes = append(nodes, n)
		return true
	})
	return nodes
}

// statements is the statement list n holds directly: a block's, or a case's.
func statements(n ast.Node) []ast.Stmt {
	switch x := n.(type) {
	case *ast.BlockStmt:
		return x.List
	case *ast.CaseClause:
		return x.Body
	case *ast.CommClause:
		return x.Body
	}
	return nil
}

func hasGoto(fd *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BranchStmt); ok && b.Tok == token.GOTO {
			found = true
		}
		return !found
	})
	return found
}

// refusal is the formatter's parse s makes whose error ends the function: `if _, err := <call>;
// err != nil { … return }`, or `_, err := <call>` (or `=`) with next `if err != nil { … return }`;
// nil otherwise.
func (a *analysis) refusal(s, next ast.Stmt) *ast.CallExpr {
	switch x := s.(type) {
	case *ast.IfStmt:
		if c, err := a.errorOf(x.Init); c != nil && a.testsError(x, err) {
			return c
		}
	case *ast.AssignStmt:
		if c, err := a.errorOf(x); c != nil {
			if test, ok := next.(*ast.IfStmt); ok && test.Init == nil && a.testsError(test, err) {
				return c
			}
		}
	}
	return nil
}

// errorOf is the formatter's parse s assigns and the variable it puts the parse's error in, when s
// is `x, err := <call>` or the same with `=`.
func (a *analysis) errorOf(s ast.Stmt) (*ast.CallExpr, types.Object) {
	assign, ok := s.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 || (assign.Tok != token.DEFINE && assign.Tok != token.ASSIGN) {
		return nil, nil
	}
	c := a.formatterCall(assign.Rhs[0])
	if c == nil {
		return nil, nil
	}
	results := calleeOf(c, a.m.info).Type().(*types.Signature).Results().Len()
	if len(assign.Lhs) != results {
		return nil, nil
	}
	id, ok := ast.Unparen(assign.Lhs[results-1]).(*ast.Ident)
	if !ok {
		return nil, nil
	}
	obj := a.object(id)
	if obj == nil {
		return nil, nil
	}
	return c, obj
}

// testsError reports whether the if statement x is `if err != nil { … return }`, with no else.
func (a *analysis) testsError(x *ast.IfStmt, err types.Object) bool {
	id, ok := ast.Unparen(nilTested(x)).(*ast.Ident)
	return ok && a.object(id) == err && endsFunction(x)
}

// nilTested is what x's condition compares with nil (`<e> != nil` or `nil != <e>`), or nil.
func nilTested(x *ast.IfStmt) ast.Expr {
	cond, ok := ast.Unparen(x.Cond).(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return nil
	}
	isNil := func(e ast.Expr) bool { id, ok := ast.Unparen(e).(*ast.Ident); return ok && id.Name == "nil" }
	switch {
	case isNil(cond.Y):
		return cond.X
	case isNil(cond.X):
		return cond.Y
	}
	return nil
}

// endsFunction reports whether the if statement x has no else and its body ends with a return.
func endsFunction(x *ast.IfStmt) bool {
	if x.Else != nil || len(x.Body.List) == 0 {
		return false
	}
	_, ok := x.Body.List[len(x.Body.List)-1].(*ast.ReturnStmt)
	return ok
}

// formatterCall is e when it is a parse of the formatter (formatterParses) with the value as its
// second argument, and nil otherwise.
func (a *analysis) formatterCall(e ast.Expr) *ast.CallExpr {
	if e == nil {
		return nil
	}
	c, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(c.Args) < 2 {
		return nil
	}
	fn := calleeOf(c, a.m.info)
	if fn == nil || !formatterParses[funcName(fn)] {
		return nil
	}
	return c
}

// object is what id declares or names.
func (a *analysis) object(id *ast.Ident) types.Object {
	if obj := a.m.info.Defs[id]; obj != nil {
		return obj
	}
	return a.m.info.Uses[id]
}

// fieldPath is e as a variable and the fields selected from it (cfg.DatabaseURL is cfg and
// [DatabaseURL]), with ok false when e is no such path or a selection in it goes through a
// pointer, which another name can write.
func (a *analysis) fieldPath(e ast.Expr) (root types.Object, fields []string, ok bool) {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		obj, isVar := a.object(x).(*types.Var)
		return obj, nil, isVar
	case *ast.SelectorExpr:
		sel := a.m.info.Selections[x]
		if sel == nil || sel.Kind() != types.FieldVal || sel.Indirect() {
			return nil, nil, false
		}
		root, fields, ok := a.fieldPath(x.X)
		return root, append(fields, x.Sel.Name), ok
	}
	return nil, nil, false
}

// overlaps reports whether a write of target can change the value e names: target is e, or a field
// path that holds e, or one e holds. An index or a dereference writes its base.
func (a *analysis) overlaps(target, e ast.Expr) bool {
	for {
		switch x := ast.Unparen(target).(type) {
		case *ast.IndexExpr:
			target = x.X
			continue
		case *ast.IndexListExpr:
			target = x.X
			continue
		case *ast.StarExpr:
			target = x.X
			continue
		}
		break
	}
	for {
		root, fields, ok := a.fieldPath(target)
		if ok {
			eRoot, eFields, _ := a.fieldPath(e)
			if root != eRoot {
				return false
			}
			for i := range min(len(fields), len(eFields)) {
				if fields[i] != eFields[i] {
					return false
				}
			}
			return true
		}
		// A selection through a pointer writes what its base's path names, or an alias of it.
		sel, isSel := ast.Unparen(target).(*ast.SelectorExpr)
		if !isSel {
			return false
		}
		target = sel.X
	}
}

// unwritten reports whether no line of fd can change the value e names between the formatter's
// statement, which ends at from, and pos: e is a variable of fd's own (or a field path of one) that
// no pointer reaches (its address, by & or a method with a pointer receiver, is not taken before
// pos or where a write would count), and no write of it falls after from and before pos, in a
// function literal, or in a loop of inside (the nodes that hold pos, below the block that holds
// both), which runs pos again after the write.
func (a *analysis) unwritten(fd *ast.FuncDecl, e ast.Expr, from, pos token.Pos, inside []ast.Node) bool {
	root, _, ok := a.fieldPath(e)
	if !ok || root.Pos() < fd.Pos() || root.Pos() >= fd.End() {
		return false
	}
	var loop ast.Node
	for _, n := range inside {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			if loop == nil {
				loop = n
			}
		}
	}
	written := false
	var literals []*ast.FuncLit
	between := func(at token.Pos) bool {
		if at >= from && at < pos {
			return true
		}
		if loop != nil && at >= loop.Pos() && at < loop.End() {
			return true
		}
		for _, l := range literals {
			if at >= l.Pos() && at < l.End() {
				return true
			}
		}
		return false
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if l, ok := n.(*ast.FuncLit); ok {
			literals = append(literals, l)
		}
		return true
	})
	write := func(target ast.Expr, at token.Pos) {
		if target != nil && a.overlaps(target, e) && between(at) {
			written = true
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				write(lhs, x.Pos())
			}
		case *ast.IncDecStmt:
			write(x.X, x.Pos())
		case *ast.RangeStmt:
			if x.Tok == token.ASSIGN {
				write(x.Key, x.Pos())
				write(x.Value, x.Pos())
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND && a.overlaps(x.X, e) && (x.Pos() < pos || between(x.Pos())) {
				written = true
			}
		case *ast.SelectorExpr:
			// A method with a pointer receiver, called on an addressable value, takes its address.
			if sel := a.m.info.Selections[x]; sel != nil && sel.Kind() == types.MethodVal {
				if recv := sel.Obj().Type().(*types.Signature).Recv(); recv != nil {
					_, pointerRecv := recv.Type().(*types.Pointer)
					_, pointerBase := a.m.info.TypeOf(x.X).(*types.Pointer)
					if pointerRecv && !pointerBase && a.overlaps(x.X, e) && (x.Pos() < pos || between(x.Pos())) {
						written = true
					}
				}
			}
		}
		return !written
	})
	return !written
}

// same reports whether x and y name the same variable, or the same field path of one.
func (a *analysis) same(x, y ast.Expr) bool {
	x, y = ast.Unparen(x), ast.Unparen(y)
	switch xx := x.(type) {
	case *ast.Ident:
		yy, ok := y.(*ast.Ident)
		if !ok {
			return false
		}
		ox, oy := a.m.info.Uses[xx], a.m.info.Uses[yy]
		return ox != nil && ox == oy
	case *ast.SelectorExpr:
		yy, ok := y.(*ast.SelectorExpr)
		return ok && xx.Sel.Name == yy.Sel.Name && a.same(xx.X, yy.X)
	case *ast.StarExpr:
		yy, ok := y.(*ast.StarExpr)
		return ok && a.same(xx.X, yy.X)
	}
	return false
}

func recvType(fn *types.Func) types.Type {
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		return recv.Type()
	}
	return nil
}

// openConnection reports whether t is a pool or a connection of pgx, pgxpool or pgconn.
func openConnection(t types.Type) bool {
	if t == nil {
		return false
	}
	if pointer, ok := t.(*types.Pointer); ok {
		t = pointer.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || !strings.HasPrefix(named.Obj().Pkg().Path(), "github.com/jackc/") {
		return false
	}
	switch named.Obj().Pkg().Name() + "." + named.Obj().Name() {
	case "pgxpool.Pool", "pgx.Conn", "pgconn.PgConn":
		return true
	}
	return false
}

// calleeOf is the function or method c calls, when the type checker names one.
func calleeOf(c *ast.CallExpr, info *types.Info) *types.Func {
	switch fun := ast.Unparen(c.Fun).(type) {
	case *ast.Ident:
		f, _ := info.Uses[fun].(*types.Func)
		return f
	case *ast.SelectorExpr:
		if sel := info.Selections[fun]; sel != nil {
			f, _ := sel.Obj().(*types.Func)
			return f
		}
		f, _ := info.Uses[fun.Sel].(*types.Func)
		return f
	case *ast.IndexExpr:
		if id, ok := fun.X.(*ast.Ident); ok {
			f, _ := info.Uses[id].(*types.Func)
			return f
		}
	}
	return nil
}

func (a *analysis) args(c *ast.CallExpr, positions []int) string {
	var parts []string
	for _, at := range positions {
		if at < len(c.Args) {
			parts = append(parts, a.text(c.Args[at]))
		}
	}
	return strings.Join(parts, ", ")
}

// text is n's source with its white space folded: whole, since a judgement names a call by it.
func (a *analysis) text(n ast.Node) string {
	start, end := a.m.fset.Position(n.Pos()), a.m.fset.Position(n.End())
	src := a.m.sources[start.Filename]
	if end.Offset > len(src) || start.Offset > end.Offset {
		return "?"
	}
	return strings.Join(strings.Fields(string(src[start.Offset:end.Offset])), " ")
}

// short is text cut to 120 bytes, for the words of a verdict.
func (a *analysis) short(n ast.Node) string {
	t := a.text(n)
	if len(t) > 120 {
		t = t[:120] + "…"
	}
	return t
}

// record is a call as the program prints it.
type record struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function"`
	Callee   string `json:"callee"`
	Argument string `json:"argument"`
	Kind     string `json:"kind"`
	Verdict  string `json:"verdict"`
	Why      string `json:"why"`
}

func (a *analysis) record(c *call) record {
	p := a.m.fset.Position(c.at)
	rel, err := filepath.Rel(a.m.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	verdict := "unjudged"
	switch {
	case c.kind == "wrapper":
		verdict = "wrapper"
	case c.verdict.judged:
		verdict = "judged"
	}
	return record{File: filepath.ToSlash(rel), Line: p.Line, Function: c.function, Callee: c.callee, Argument: c.argument, Kind: c.kind, Verdict: verdict, Why: c.verdict.why}
}
