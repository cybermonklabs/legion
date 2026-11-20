package main

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

// fieldsConfig is the field census's configuration (gate.boundary_fields): the boundary's entry
// points, the functions whose answer is the rule's reading, the parses whose first answer is a
// value read from text and no text, and the fields the boundary sets from a value the census
// cannot read as closed or ruled, each with why that is safe.
type fieldsConfig struct {
	Entry  []string    `json:"entry"`  // types.Func.FullName of each entry point, all in one package
	Rule   []string    `json:"rule"`   // functions whose answer is the rule's reading
	Parses []string    `json:"parses"` // functions whose first answer is a parsed value
	Held   []heldField `json:"held"`
}

// heldField is a field the boundary sets from a value that is neither closed nor the rule's
// reading, in one function, and why that value prints nowhere.
type heldField struct {
	Function string `json:"function"`
	Field    string `json:"field"` // the struct type's pkg path and name, then the field: pkg.T.f
	Why      string `json:"why"`
}

// fieldCensus is the field census: it lists each field the boundary sets - a struct literal's
// field, a map or slice literal's element, or an assignment to a field - inside the boundary's
// functions, and classifies the value set there. The boundary's functions are its entry points
// (gate.boundary_fields.entry), every function of their package one of them calls, and every
// method of a type of that package one of them builds, but not the rule's own functions, the
// call census's (gate.rule, gate.printers), or the box's methods, which the fixture, the call
// census and the box census hold. A value passes when it is a constant, a value of a type that
// can hold no text (textual), the box, the rule's reading (a call of gate.boundary_fields.rule),
// a method of the box, a literal built there (whose own elements are listed), the answer of one
// of the boundary's functions that answers no string, the first answer of a parse
// (gate.boundary_fields.parses), or a call whose receiver and every argument pass; a local
// variable is read through its one definition. Any other value - a field of the answer, a
// parameter, a string a call made of one - is a remote string set where the rule has not read it,
// and fails unless gate.boundary_fields.held names that field in that function. A census that
// found no entry point, or listed nothing, is UNKNOWN.
func (g *gate) fieldCensus(c *census, dir, path string, fset *token.FileSet, files []*ast.File, info *types.Info) {
	cfg := g.inv.Gate.BoundaryFields
	decls := map[string]*ast.FuncDecl{}
	declFile := map[string]string{}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, fset.Position(f.Pos()).Filename)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				decls[goFuncName(path, fd)] = fd
				declFile[goFuncName(path, fd)] = filepath.ToSlash(rel)
			}
		}
	}
	boxPkg, boxName := splitTypeName(g.inv.Gate.Box.Type)
	isBox := func(t types.Type) bool {
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == boxPkg && named.Obj().Name() == boxName
	}
	held := set(append(append([]string{}, g.inv.Gate.Rule...), g.inv.Gate.Printers...))
	for _, r := range cfg.Rule {
		held[r] = true
	}
	// The scope: the entry points, and what they call and build in their package.
	scope := map[string]bool{}
	var queue []string
	add := func(name string) {
		if _, declared := decls[name]; declared && !scope[name] && !held[name] {
			scope[name] = true
			queue = append(queue, name)
		}
	}
	for _, e := range cfg.Entry {
		if _, ok := decls[e]; !ok {
			c.Unknown = append(c.Unknown, fmt.Sprintf("field census: the entry point %s is not declared in %s", e, path))
			continue
		}
		add(e)
	}
	built := map[*types.Named]bool{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		ast.Inspect(decls[name], func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if fn, ok := info.Uses[n].(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == path {
					if recv := fn.Type().(*types.Signature).Recv(); recv == nil || !isBox(recv.Type()) {
						add(fn.Origin().FullName())
					}
				}
			case *ast.CompositeLit:
				t := info.Types[n].Type
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if named, ok := t.(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && !isBox(named) && !built[named] {
					built[named] = true
					for i := range named.NumMethods() {
						add(named.Method(i).FullName())
					}
				}
			}
			return true
		})
	}
	listed := 0
	for name := range scope {
		fd := decls[name]
		j := &fieldJudge{info: info, rule: set(cfg.Rule), parses: set(cfg.Parses), scope: scope, isBox: isBox, defs: localDefinitions(fd, info)}
		record := func(at token.Pos, field string, value ast.Expr, ok bool, why string) {
			listed++
			if !ok {
				for _, h := range cfg.Held {
					if h.Function == name && h.Field == field {
						ok, why = true, "held: "+h.Why
					}
				}
			}
			c.Fields = append(c.Fields, finding{File: declFile[name], Line: fset.Position(at).Line, Function: name, What: field + " = " + exprString(value), Kind: "field", OK: ok, Why: why})
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				t := info.Types[n].Type
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				typeName := types.TypeString(t, nil)
				for i, elt := range n.Elts {
					key, value := "", elt
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						key, value = types.ExprString(kv.Key), kv.Value
					}
					under, isStruct := t.Underlying().(*types.Struct)
					switch {
					case isStruct && isBox(t):
						record(elt.Pos(), typeName+"."+cmp.Or(key, under.Field(i).Name()), value, true, "the box's own storage, which the box census holds")
					case isStruct:
						ok, why := j.classify(value, 0)
						record(elt.Pos(), typeName+"."+cmp.Or(key, under.Field(i).Name()), value, ok, why)
					default:
						ok, why := j.classify(value, 0)
						record(elt.Pos(), typeName+" element "+cmp.Or(key, fmt.Sprint(i)), value, ok, why)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok || i >= len(n.Rhs) {
						continue
					}
					field, ok := info.Uses[sel.Sel].(*types.Var)
					if !ok || !field.IsField() {
						continue
					}
					owner := strings.TrimPrefix(types.TypeString(info.Types[sel.X].Type, nil), "*")
					ok, why := j.classify(n.Rhs[i], 0)
					record(n.Pos(), owner+"."+field.Name(), n.Rhs[i], ok, why)
				}
			}
			return true
		})
	}
	if len(scope) > 0 && listed == 0 {
		c.Unknown = append(c.Unknown, "field census: it listed no field the boundary sets in "+path)
	}
}

// exprString is types.ExprString, with a literal whose type its parent gives written {…}.
func exprString(e ast.Expr) string {
	if lit, ok := e.(*ast.CompositeLit); ok && lit.Type == nil {
		return "{…}"
	}
	return types.ExprString(e)
}

// definition is how a local variable got its one value: the expression, and, where the
// expression answers several values, which of them.
type definition struct {
	value ast.Expr
	index int
}

// localDefinitions is each local variable of fd defined once, by `x := e` or `x, y := f()`, and
// never assigned again, with its definition.
func localDefinitions(fd *ast.FuncDecl, info *types.Info) map[*types.Var]definition {
	defs := map[*types.Var]definition{}
	assigned := map[*types.Var]int{}
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := ast.Unparen(lhs).(*ast.Ident)
			if !ok {
				continue
			}
			v, ok := info.ObjectOf(id).(*types.Var)
			if !ok {
				continue
			}
			assigned[v]++
			switch {
			case as.Tok != token.DEFINE:
			case len(as.Lhs) == len(as.Rhs):
				defs[v] = definition{value: as.Rhs[i], index: -1}
			case len(as.Rhs) == 1:
				defs[v] = definition{value: as.Rhs[0], index: i}
			}
		}
		return true
	})
	for v := range defs {
		if assigned[v] != 1 {
			delete(defs, v)
		}
	}
	return defs
}

// fieldJudge classifies the value the boundary sets in a field (fieldCensus).
type fieldJudge struct {
	info   *types.Info
	rule   map[string]bool
	parses map[string]bool
	scope  map[string]bool
	isBox  func(types.Type) bool
	defs   map[*types.Var]definition
}

// classify answers whether value is closed or the rule's reading, and what it is.
func (j *fieldJudge) classify(value ast.Expr, depth int) (bool, string) {
	value = ast.Unparen(value)
	tv := j.info.Types[value]
	switch {
	case depth > 8:
		return false, "a value read through too many definitions to classify"
	case tv.Value != nil || tv.IsNil():
		return true, "a constant"
	case tv.Type != nil && j.isBox(tv.Type):
		return true, "the box, which prints only the rule's reading"
	case tv.Type != nil && !textual(tv.Type, map[types.Type]bool{}):
		return true, "a closed value of type " + types.TypeString(tv.Type, nil)
	}
	switch v := value.(type) {
	case *ast.CompositeLit:
		return true, "a literal built here, whose elements this census lists"
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return j.classify(v.X, depth+1)
		}
	case *ast.Ident:
		def, ok := j.defs[j.varOf(v)]
		switch {
		case !ok:
		case def.index < 0:
			ok, why := j.classify(def.value, depth+1)
			return ok, fmt.Sprintf("%s, defined as %s: %s", v.Name, exprString(def.value), why)
		case def.index == 0:
			if call, isCall := ast.Unparen(def.value).(*ast.CallExpr); isCall {
				if fn := j.callee(call); fn != nil && j.parses[fn.Origin().FullName()] {
					return true, fmt.Sprintf("%s, the value %s parses, which holds no text", v.Name, fn.Origin().FullName())
				}
			}
		}
	case *ast.CallExpr:
		if fun, ok := j.info.Types[v.Fun]; ok && fun.IsType() && len(v.Args) == 1 {
			ok, why := j.classify(v.Args[0], depth+1)
			return ok, "a conversion of " + why
		}
		if fn := j.callee(v); fn != nil {
			full := fn.Origin().FullName()
			sig := fn.Type().(*types.Signature)
			switch {
			case j.rule[full]:
				return true, "the rule's reading (" + full + ")"
			case sig.Recv() != nil && j.isBox(sig.Recv().Type()):
				return true, "a method of the box (" + full + "), which the box census holds"
			case j.scope[full] && !answersString(sig):
				return true, "the answer of the boundary's own " + full + ", whose fields this census lists"
			}
		}
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			if s := j.info.Selections[sel]; s != nil && s.Kind() == types.MethodVal {
				if ok, _ := j.classify(sel.X, depth+1); !ok {
					return false, fmt.Sprintf("%s, a call on %s, a value that has not gone through the rule", exprString(v), exprString(sel.X))
				}
			}
		}
		for _, arg := range v.Args {
			if ok, _ := j.classify(arg, depth+1); !ok {
				return false, fmt.Sprintf("%s, a call of %s, a value that has not gone through the rule", exprString(v), exprString(arg))
			}
		}
		return true, "built from closed values by " + exprString(v.Fun)
	}
	return false, exprString(value) + ", a remote string that has not gone through the rule"
}

// varOf is the local variable id names, or nil.
func (j *fieldJudge) varOf(id *ast.Ident) *types.Var {
	v, _ := j.info.ObjectOf(id).(*types.Var)
	return v
}

// callee is the function or method call calls, past an explicit instantiation, or nil.
func (j *fieldJudge) callee(call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	if index, ok := fun.(*ast.IndexExpr); ok {
		fun = index.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		fn, _ := j.info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := j.info.Uses[f.Sel].(*types.Func)
		return fn
	}
	return nil
}

// answersString reports whether one of sig's results can hold text (textual) as a string or a
// byte slice does.
func answersString(sig *types.Signature) bool {
	for i := range sig.Results().Len() {
		switch t := sig.Results().At(i).Type().Underlying().(type) {
		case *types.Basic:
			if t.Info()&types.IsString != 0 {
				return true
			}
		case *types.Slice:
			if b, ok := t.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
				return true
			}
		}
	}
	return false
}

// textual reports whether a value of t can hold text: a string, a byte slice, an interface, a
// slice, array or map of what can, or a pointer to or a struct with any field that can, exported
// or not (a strings.Reader holds its string unexported). A number, a bool, a function and a
// channel cannot.
func textual(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&types.IsString != 0
	case *types.Interface:
		return true
	case *types.Slice:
		if b, ok := u.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
			return true
		}
		return textual(u.Elem(), seen)
	case *types.Array:
		return textual(u.Elem(), seen)
	case *types.Map:
		return textual(u.Key(), seen) || textual(u.Elem(), seen)
	case *types.Pointer:
		return textual(u.Elem(), seen)
	case *types.Struct:
		for i := range u.NumFields() {
			if textual(u.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}
