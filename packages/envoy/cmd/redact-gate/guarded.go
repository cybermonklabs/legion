package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strings"
)

// guardedPackageRead judges a reflection whose inventory entry names a guarded package P
// (guarded_package): a read of a value whose type no static type names, such as a library's
// unexported handle behind an interface. This is the one statement of the read such an entry
// admits; main.go's reflectionExempt, the reflection census and the check script cite it. The
// census checks the read and P in place of the types the read reaches, and admits the read only
// where:
//
//  1. it is a reflect.Value.FieldByName with a constant name: any other reflect call, FieldByIndex
//     and Field among them, a name that is no constant, and a conversion are refused;
//  2. its receiver X is a variable of the function it is called in, which one statement of the
//     package stores to, before the guard, and none takes the address of; and the guard, a
//     statement of that function's body before the one holding the read, is
//     `if ... || X.Type().PkgPath() != P || ... { ...; return ... }`, with no init and no else, in
//     a function with no label, so no goto passes it. P is written as reflect.TypeFor[T]().PkgPath()
//     for a named type T of P, in place or as the one initializer of an unexported package-level
//     variable no other statement of the package stores to; an exported variable can be stored to
//     by any package that imports it (packageGuarded, pkgPathOf);
//  3. its result is only the receiver of methods that answer nothing or closed values, or the value
//     of one variable stored once whose every use is such a receiver (resultReceiver). A closed
//     value holds no value of another type and no address (closedValue): a boolean, a number other
//     than a uintptr, a string, a defined type over one (reflect.Kind), or a slice or an array of
//     closed values; a reflect.Value, an interface (reflect.Type among them), a func, a pointer, a
//     map, a channel, a struct, a uintptr and an unsafe.Pointer are none. So these methods of
//     reflect.Value, each of which reaches past the field's own value, are refused on the result,
//     and this is the one list of them, which a test holds to the toolchain's reflect: Elem and
//     Interface, what an interface or a pointer holds; Call and CallSlice, a func field's results;
//     Method, MethodByName and Methods, a method's, and Type the same through its Method(i).Func;
//     Field, Fields, FieldByIndex, FieldByIndexErr, FieldByName, FieldByNameFunc, Index, MapIndex,
//     MapKeys, MapRange, Recv, TryRecv, Seq, Seq2, Slice and Slice3, a member or an element; Addr
//     and Convert, another view of the field, whose own methods the census would not read; and
//     Pointer, UnsafeAddr, UnsafePointer and InterfaceData, an address;
//  4. P's import closure under the builds listed so far (recordImports) holds neither the box's
//     package nor an external error type's (closureRefused), so no type P declares can name the
//     box;
//  5. P, type-checked from its source for the build being read, with no cgo files (sourceChecked),
//     declares no generic type: an instance of one is P's to reflect (PkgPath) and holds whatever
//     its type argument is, the box among them, which P's import closure does not show;
//  6. in every struct type P's source declares, at package level or in a function body, each field
//     of the read's name that FieldByName can reach (fieldsNamed: the struct's own, and those of
//     every struct it embeds, by value or by pointer, from any package, at any depth, one
//     FieldByName does not return, shadowed or ambiguous, included) is typed with no interface and
//     no type parameter at any depth (holdsDynamic), and at least one struct type reaches a field of
//     that name (fieldRead): only an interface or a type parameter could hold the box at run time.
//
// It assumes, and the check script says it does not establish, that a func type holds nothing and
// that no type's methods need reading: a func's captured values are out of reflect's reach, and
// its results and a method's are reached from the read's result only through a method condition 3
// refuses.
func (w *walker) guardedPackageRead(call *ast.CallExpr, parents []ast.Node, kind, what, named string) (bool, string) {
	switch {
	case kind != "reflect":
		return false, "guarded_package names a conversion, which no type guard holds"
	case what != "(reflect.Value).FieldByName":
		return false, "guarded_package admits a reflect.Value.FieldByName with a constant name alone, and this is " + what
	}
	pkg, why := w.packageGuarded(call, parents)
	switch {
	case pkg == "":
		return false, "guarded_package names a reflection that does not run only under a guard holding its value's type to one package: " + why
	case pkg != named:
		return false, "guarded_package names " + named + ", and the guard here holds the value's type to " + pkg
	}
	if why := w.closureRefused(pkg); why != "" {
		return false, why
	}
	declared := w.g.sourceChecked(pkg, w.build)
	if declared.err != "" {
		return false, "guarded_package names " + pkg + ", whose source cannot be type-checked: " + declared.err
	}
	if generic := declared.genericTypes(); len(generic) > 0 {
		return false, "guarded_package names " + pkg + ", which declares the generic type " + strings.Join(generic, ", ") + ": an instance is " + pkg + "'s to reflect and holds whatever its type argument is, the box among them, which no import closure shows"
	}
	read, why := w.fieldRead(call, parents, pkg, declared)
	if !read {
		return false, why
	}
	return true, "its guard holds its value's type to " + pkg + ", whose import closure holds neither the box's package nor an external error type's, and which declares no generic type; " + why
}

// collectStores is, for every variable of the package's files, where a statement stores to it
// (an assignment, an increment, a range clause, a declaration with a value) and, counted as a store
// at each, where its address is taken; and the initializer of each package-level variable
// declared alone with one value. A guarded_package entry's guard reads both (packageGuarded).
func collectStores(files []*ast.File, info *types.Info) (map[types.Object][]token.Pos, map[types.Object]ast.Expr) {
	stores := map[types.Object][]token.Pos{}
	inits := map[types.Object]ast.Expr{}
	store := func(e ast.Expr, at token.Pos) {
		if id, ok := ast.Unparen(e).(*ast.Ident); ok {
			if o := info.Defs[id]; o != nil {
				stores[o] = append(stores[o], at)
			} else if o := info.Uses[id]; o != nil {
				stores[o] = append(stores[o], at)
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					store(l, n.Pos())
				}
			case *ast.IncDecStmt:
				store(n.X, n.Pos())
			case *ast.RangeStmt:
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if e != nil {
						store(e, n.Pos())
					}
				}
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					store(n.X, n.Pos())
				}
			case *ast.ValueSpec:
				if len(n.Values) > 0 {
					for _, name := range n.Names {
						store(name, n.Pos())
					}
				}
			}
			return true
		})
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
				for _, s := range gd.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok && len(vs.Names) == 1 && len(vs.Values) == 1 {
						if o := info.Defs[vs.Names[0]]; o != nil {
							inits[o] = vs.Values[0]
						}
					}
				}
			}
		}
	}
	return stores, inits
}

// enclosingBody is the body of the innermost function among parents, a declaration or a literal,
// or nil.
func enclosingBody(parents []ast.Node) *ast.BlockStmt {
	for i := len(parents) - 1; i >= 0; i-- {
		switch fn := parents[i].(type) {
		case *ast.FuncDecl:
			return fn.Body
		case *ast.FuncLit:
			return fn.Body
		}
	}
	return nil
}

// packageGuarded is the package P that the guard of guardedPackageRead's condition 2 holds the
// type of call's receiver to, or "" and why there is none.
func (w *walker) packageGuarded(call *ast.CallExpr, parents []ast.Node) (string, string) {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", "the call has no receiver"
	}
	id, _ := ast.Unparen(sel.X).(*ast.Ident)
	var value *types.Var
	if id != nil {
		value, _ = w.info.Uses[id].(*types.Var)
	}
	body := enclosingBody(parents)
	if value == nil || body == nil || value.Pos() < body.Pos() || value.Pos() > body.End() {
		return "", "its receiver is no variable of the function it is called in"
	}
	labeled := false
	ast.Inspect(body, func(n ast.Node) bool {
		_, isLabel := n.(*ast.LabeledStmt)
		labeled = labeled || isLabel
		return !labeled
	})
	if labeled {
		return "", "the function holds a label, so a goto can pass a guard"
	}
	stores := w.stores[value]
	if len(stores) != 1 {
		return "", fmt.Sprintf("its receiver %s is stored to %d times, its address taken counted as one", id.Name, len(stores))
	}
	for _, stmt := range body.List {
		if stmt.End() > call.Pos() {
			break
		}
		guard, ok := stmt.(*ast.IfStmt)
		if !ok || guard.Init != nil || guard.Else != nil || stores[0] > guard.Pos() || len(guard.Body.List) == 0 {
			continue
		}
		if _, returns := guard.Body.List[len(guard.Body.List)-1].(*ast.ReturnStmt); !returns {
			continue
		}
		if pkg := w.pkgPathGuard(guard.Cond, value); pkg != "" {
			return pkg, ""
		}
	}
	return "", "no statement before it, after " + id.Name + " is stored, returns where " + id.Name + ".Type().PkgPath() is not one named type's package"
}

// pkgPathGuard is the package cond, read as a chain of ||, compares the package path of value's
// type with by != (pkgPathOf), or "".
func (w *walker) pkgPathGuard(cond ast.Expr, value types.Object) string {
	b, ok := ast.Unparen(cond).(*ast.BinaryExpr)
	if !ok {
		return ""
	}
	switch b.Op {
	case token.LOR:
		if pkg := w.pkgPathGuard(b.X, value); pkg != "" {
			return pkg
		}
		return w.pkgPathGuard(b.Y, value)
	case token.NEQ:
		if w.typePkgPath(b.X, value) {
			return w.pkgPathOf(b.Y)
		}
		if w.typePkgPath(b.Y, value) {
			return w.pkgPathOf(b.X)
		}
	}
	return ""
}

// typePkgPath reports whether e is value.Type().PkgPath(), value a reflect.Value.
func (w *walker) typePkgPath(e ast.Expr, value types.Object) bool {
	pkgPath, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(pkgPath.Args) != 0 || w.callee(pkgPath) != "(reflect.Type).PkgPath" {
		return false
	}
	sel, ok := ast.Unparen(pkgPath.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	typeOf, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || len(typeOf.Args) != 0 || w.callee(typeOf) != "(reflect.Value).Type" {
		return false
	}
	sel, ok = ast.Unparen(typeOf.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := ast.Unparen(sel.X).(*ast.Ident)
	return ok && w.info.Uses[id] == value
}

// pkgPathOf is the package e names as guardedPackageRead's condition 2 writes P: e itself, or an
// unexported package-level variable whose one store (collectStores) is that initializer; "" for
// any other expression.
func (w *walker) pkgPathOf(e ast.Expr) string {
	if id, ok := ast.Unparen(e).(*ast.Ident); ok {
		v, ok := w.info.Uses[id].(*types.Var)
		if !ok || v.Exported() || v.Pkg() == nil || v.Parent() != v.Pkg().Scope() || len(w.stores[v]) != 1 || w.inits[v] == nil {
			return ""
		}
		e = w.inits[v]
	}
	pkgPath, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(pkgPath.Args) != 0 || w.callee(pkgPath) != "(reflect.Type).PkgPath" {
		return ""
	}
	sel, ok := ast.Unparen(pkgPath.Fun).(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	typeFor, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || len(typeFor.Args) != 0 {
		return ""
	}
	fun := ast.Unparen(typeFor.Fun)
	if index, ok := fun.(*ast.IndexExpr); ok {
		fun = ast.Unparen(index.X)
	}
	var name *ast.Ident
	switch fun := fun.(type) {
	case *ast.SelectorExpr:
		name = fun.Sel
	case *ast.Ident:
		name = fun
	}
	if fn, ok := w.info.Uses[name].(*types.Func); !ok || fn.Origin().FullName() != "reflect.TypeFor" {
		return ""
	}
	instance := w.info.Instances[name]
	if instance.TypeArgs == nil || instance.TypeArgs.Len() != 1 {
		return ""
	}
	named, ok := types.Unalias(instance.TypeArgs.At(0)).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path()
}

// recordImports adds deps, the import closure go list names for path under one build, and path
// itself, to path's closure: the union over the builds listed so far, so the closure a guard is
// checked against (closureRefused) is never narrower than the build being read.
func (g *gate) recordImports(path string, deps []string) {
	if g.imports == nil {
		g.imports = map[string]map[string]bool{}
	}
	closure := g.imports[path]
	if closure == nil {
		closure = map[string]bool{path: true}
		g.imports[path] = closure
	}
	for _, d := range deps {
		closure[d] = true
	}
}

// closureRefused is why pkg's import closure (recordImports) fails guardedPackageRead's condition
// 4, or "" where it holds neither the box's package nor an external error type's.
func (w *walker) closureRefused(pkg string) string {
	closure := w.g.imports[pkg]
	if closure == nil {
		return "guarded_package names " + pkg + ", whose import closure no go list of these builds names"
	}
	var held []string
	if closure[w.boxPkg] {
		held = append(held, "the box's package "+w.boxPkg)
	}
	for _, e := range w.g.inv.Gate.External {
		if p, _ := splitTypeName(e); closure[p] {
			held = append(held, "the package of the external error type "+e)
		}
	}
	if len(held) > 0 {
		return "guarded_package names " + pkg + ", whose import closure holds " + strings.Join(held, " and ")
	}
	return ""
}

// fieldRead reports whether call, a guarded FieldByName on a value of a type of pkg, whose source
// is declared, meets guardedPackageRead's condition 1 for its name and its conditions 3 and 6, and
// says why.
func (w *walker) fieldRead(call *ast.CallExpr, parents []ast.Node, pkg string, declared *checkedSource) (bool, string) {
	tv := w.info.Types[call.Args[0]]
	if tv.Value == nil || tv.Value.Kind() != constant.String {
		return false, "guarded_package admits a FieldByName whose name is a constant, and this one's is not"
	}
	name := constant.StringVal(tv.Value)
	if why := w.resultReceiver(call, parents); why != "" {
		return false, "guarded_package admits a FieldByName whose result is only the receiver of methods that answer nothing or closed values: " + why
	}
	var found int
	var dynamic []string
	for _, tn := range declared.declaredTypes() {
		s, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		reached := fieldsNamed(s, name, nil, map[*types.Struct]bool{})
		if len(reached) > 0 {
			found++
		}
		for _, r := range reached {
			if !holdsDynamic(r.field.Type(), map[types.Type]bool{}) {
				continue
			}
			held := tn.Name() + "." + name + " (" + typeString(r.field.Type()) + ")"
			if len(r.through) > 0 {
				embedded := make([]string, len(r.through))
				for i, e := range r.through {
					embedded[i] = typeString(e.Type())
				}
				held += ", promoted through the embedded " + strings.Join(embedded, ", then ")
			}
			dynamic = append(dynamic, held)
		}
	}
	switch {
	case found == 0:
		return false, "no struct type " + pkg + " declares has a field " + name + ", so the census reads nothing of what the read reaches"
	case len(dynamic) > 0:
		return false, pkg + " types the field " + name + " with an interface or a type parameter, which can hold the box at run time: " + strings.Join(dynamic, "; ")
	}
	return true, fmt.Sprintf("FieldByName(%q) reaches no interface and no type parameter in any of the %d struct types %s declares that hold a field of that name, and its result is only the receiver of methods that answer closed values", name, found, pkg)
}

// reachedField is a field FieldByName can reach in a struct type, and the embedded fields it is
// promoted through, outermost first.
type reachedField struct {
	field   *types.Var
	through []*types.Var
}

// fieldsNamed is every field named name that reflect's FieldByName can reach in a value of the
// struct type s, reached through the embedded fields through: s's own fields, and those of each
// struct type s embeds, by value or by pointer, at any depth. reflect matches a field by its name's
// text alone, whatever package declares it, so an unexported field of another package's struct
// that s embeds is reached, as go/types' LookupFieldOrMethod, which matches an unexported name only
// in the package it is given, does not. Each struct type is read once (seen), so a cycle of
// embedded pointers ends the walk. FieldByName returns the shallowest of these, and none where two
// share that depth; every one is listed, so a field it does not return is held to what one it
// returns is.
func fieldsNamed(s *types.Struct, name string, through []*types.Var, seen map[*types.Struct]bool) []reachedField {
	if seen[s] {
		return nil
	}
	seen[s] = true
	var out []reachedField
	for i := range s.NumFields() {
		f := s.Field(i)
		if f.Name() == name {
			out = append(out, reachedField{field: f, through: through})
		}
		if !f.Embedded() {
			continue
		}
		t := types.Unalias(f.Type())
		if p, ok := t.(*types.Pointer); ok {
			t = types.Unalias(p.Elem())
		}
		if embedded, ok := t.Underlying().(*types.Struct); ok {
			out = append(out, fieldsNamed(embedded, name, append(slices.Clip(through), f), seen)...)
		}
	}
	return out
}

// resultReceiver is "" where call's result is used only as guardedPackageRead's condition 3
// admits, else why not.
func (w *walker) resultReceiver(call *ast.CallExpr, parents []ast.Node) string {
	if len(parents) == 0 {
		return "its result has no use the census reads"
	}
	receiver := func(sel *ast.SelectorExpr) string {
		s := w.info.Selections[sel]
		if s == nil || s.Kind() != types.MethodVal {
			return "its result's ." + sel.Sel.Name + " is no method"
		}
		sig, _ := s.Type().(*types.Signature)
		if sig == nil {
			return "its result's ." + sel.Sel.Name + " has no signature the census reads"
		}
		var open []string
		for i := range sig.Results().Len() {
			if t := sig.Results().At(i).Type(); !closedValue(t) {
				open = append(open, typeString(t))
			}
		}
		if len(open) > 0 {
			return "its result's " + sel.Sel.Name + " reaches what the field holds, or past it: it answers " + strings.Join(open, ", ") + ", which is no closed value"
		}
		return ""
	}
	var lhs ast.Expr
	switch parent := parents[len(parents)-1].(type) {
	case *ast.SelectorExpr:
		return receiver(parent)
	case *ast.AssignStmt:
		if len(parent.Lhs) == 1 && len(parent.Rhs) == 1 && parent.Rhs[0] == call {
			lhs = parent.Lhs[0]
		}
	case *ast.ValueSpec:
		if len(parent.Names) == 1 && len(parent.Values) == 1 && parent.Values[0] == call {
			lhs = parent.Names[0]
		}
	}
	id, ok := lhs.(*ast.Ident)
	if !ok {
		return "its result is used other than as a receiver or one variable's value"
	}
	result, _ := w.info.Defs[id].(*types.Var)
	if result == nil {
		result, _ = w.info.Uses[id].(*types.Var)
	}
	if result == nil || len(w.stores[result]) != 1 {
		return "the variable that holds its result is not stored to once"
	}
	body := enclosingBody(parents)
	if body == nil {
		return "its result is held outside a function"
	}
	uses, receivers := 0, 0
	why := ""
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if w.info.Uses[n] == result {
				uses++
			}
		case *ast.SelectorExpr:
			if x, ok := ast.Unparen(n.X).(*ast.Ident); ok && w.info.Uses[x] == result {
				receivers++
				if r := receiver(n); r != "" && why == "" {
					why = r
				}
			}
		}
		return true
	})
	switch {
	case why != "":
		return why
	case uses != receivers:
		return fmt.Sprintf("the variable %s that holds its result is used %d times, %d of them as a method call's receiver", id.Name, uses, receivers)
	}
	return ""
}

// holdsDynamic reports whether a value of t can hold a value of a type t does not name: t, or what
// a pointer, slice, array, map, channel or struct field of it holds, is an interface or a type
// parameter. A func type holds none, and a type's methods are not read, as guardedPackageRead
// assumes.
func holdsDynamic(t types.Type, seen map[types.Type]bool) bool {
	return edges{chans: true}.reaches(t, func(t types.Type) bool {
		switch t.(type) {
		case *types.Interface, *types.TypeParam:
			return true
		}
		return false
	}, seen)
}

// closedValue reports whether a value of t is a closed value, as guardedPackageRead's condition 3
// defines one.
func closedValue(t types.Type) bool {
	switch t := t.Underlying().(type) {
	case *types.Basic:
		switch t.Kind() {
		case types.Uintptr, types.UnsafePointer, types.UntypedNil, types.Invalid:
			return false
		}
		return true
	case *types.Slice:
		return closedValue(t.Elem())
	case *types.Array:
		return closedValue(t.Elem())
	}
	return false
}

// checkedSource is a package type-checked from its source (sourceChecked), or why it could not be.
type checkedSource struct {
	files []*ast.File
	info  *types.Info
	err   string
}

// declaredTypes is every named type the package's source declares, at package level or in a
// function body: a type export data holds only where an exported declaration names it is here too.
func (s *checkedSource) declaredTypes() []*types.TypeName {
	var out []*types.TypeName
	for _, f := range s.files {
		ast.Inspect(f, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok {
				if tn, ok := s.info.Defs[spec.Name].(*types.TypeName); ok {
					out = append(out, tn)
				}
			}
			return true
		})
	}
	return out
}

// genericTypes are the names of the generic types the package's source declares
// (guardedPackageRead's condition 5).
func (s *checkedSource) genericTypes() []string {
	var generic []string
	for _, tn := range s.declaredTypes() {
		switch t := tn.Type().(type) {
		case *types.Named:
			if t.TypeParams().Len() > 0 {
				generic = append(generic, tn.Name())
			}
		case *types.Alias:
			if t.TypeParams().Len() > 0 {
				generic = append(generic, tn.Name())
			}
		}
	}
	return generic
}

// sourceChecked is pkg type-checked from its source files as build, the build being read, lists
// them, its imports from that build's export data (goSource); cached by package and build. Export
// data holds an unexported type only where an exported declaration names it, so the guarded
// package's own types are read from its source.
func (g *gate) sourceChecked(pkg string, build goBuild) *checkedSource {
	key := pkg
	src, listed := build[pkg]
	if listed {
		key += "\x00" + src.exports[pkg]
	}
	if checked := g.declared[key]; checked != nil {
		return checked
	}
	checked := &checkedSource{}
	if g.declared == nil {
		g.declared = map[string]*checkedSource{}
	}
	g.declared[key] = checked
	switch {
	case !listed:
		checked.err = "the go list of the build being read names no source of it"
		return checked
	case len(src.cgo) > 0:
		checked.err = "it has " + cgoUnread(src.cgo)
		return checked
	}
	fset := token.NewFileSet()
	files, _, err := src.parse(fset)
	if err != nil {
		checked.err = err.Error()
		return checked
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{Importer: src.importer(fset)}).Check(pkg, fset, files, info); err != nil {
		checked.err = err.Error()
		return checked
	}
	checked.files, checked.info = files, info
	return checked
}
