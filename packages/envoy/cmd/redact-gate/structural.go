package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// structural runs the box, holder and reflection censuses over every production file of one
// type-checked package.
func (g *gate) structural(c *census, dir, path string, fset *token.FileSet, files []*ast.File, info *types.Info) {
	box := g.inv.Gate.Box
	boxPkg, boxName := splitTypeName(box.Type)
	g.collectValueOf(path, files, info)
	stores, inits := collectStores(files, info)
	// The generic instances already listed in this package, each listed once.
	instances := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, fset.Position(f.Pos()).Filename)
		rel = filepath.ToSlash(rel)
		w := &walker{g: g, c: c, fset: fset, info: info, rel: rel, path: path, boxPkg: boxPkg, boxName: boxName, instances: instances, stores: stores, inits: inits}
		w.file(f)
	}
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

// valueOfSite is one reflect.ValueOf call, with the static types its argument can have: its own,
// or, for an interface-typed parameter of the enclosing function, those its callers in the
// package pass; resolved is nil when that cannot be read.
type valueOfSite struct {
	resolved []types.Type
}

// collectValueOf records every reflect.ValueOf call of the package at path.
func (g *gate) collectValueOf(path string, files []*ast.File, info *types.Info) {
	if g.valueOf == nil {
		g.valueOf = map[string][]valueOfSite{}
	}
	// The arguments each function of the package is called with, by parameter index.
	args := map[types.Object][][]types.Type{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var id *ast.Ident
			switch fun := ast.Unparen(call.Fun).(type) {
			case *ast.Ident:
				id = fun
			case *ast.SelectorExpr:
				id = fun.Sel
			}
			if id == nil {
				return true
			}
			fn, ok := info.Uses[id].(*types.Func)
			if !ok {
				return true
			}
			for len(args[fn]) < len(call.Args) {
				args[fn] = append(args[fn], nil)
			}
			for i, a := range call.Args {
				args[fn][i] = append(args[fn][i], info.Types[a].Type)
			}
			return true
		})
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			params := map[types.Object]int{}
			index := 0
			for _, field := range fd.Type.Params.List {
				for _, name := range field.Names {
					params[info.Defs[name]] = index
					index++
				}
				if len(field.Names) == 0 {
					index++
				}
			}
			self := info.Defs[fd.Name]
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := info.Uses[sel.Sel].(*types.Func)
				if !ok || fn.FullName() != "reflect.ValueOf" {
					return true
				}
				t := info.Types[call.Args[0]].Type
				site := valueOfSite{resolved: []types.Type{t}}
				if _, iface := t.Underlying().(*types.Interface); iface {
					site.resolved = nil
					if id, ok := ast.Unparen(call.Args[0]).(*ast.Ident); ok {
						if i, ok := params[info.Uses[id]]; ok && i < len(args[self]) && len(args[self][i]) > 0 {
							site.resolved = args[self][i]
							for _, c := range site.resolved {
								if _, iface := c.Underlying().(*types.Interface); iface {
									site.resolved = nil
									break
								}
							}
						}
					}
				}
				g.valueOf[path] = append(g.valueOf[path], site)
				return true
			})
		}
	}
}

// splitTypeName is "pkg/path.Name" as its package path and name.
func splitTypeName(full string) (string, string) {
	i := strings.LastIndex(full, ".")
	if i < 0 {
		return "", full
	}
	return full[:i], full[i+1:]
}

type walker struct {
	g               *gate
	c               *census
	fset            *token.FileSet
	info            *types.Info
	rel, path       string
	boxPkg, boxName string
	writes          int
	instances       map[string]bool
	stores          map[types.Object][]token.Pos // collectStores
	inits           map[types.Object]ast.Expr    // collectStores
}

func (w *walker) at(n ast.Node) (string, int) {
	p := w.fset.Position(n.Pos())
	return w.rel, p.Line
}

// isBox reports whether t is the box's named type.
func (w *walker) isBox(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == w.boxPkg && n.Obj().Name() == w.boxName
}

// contains reports whether a value of type t holds the box anywhere: the box itself, or through a
// pointer, a field (embedded or not), a slice, an array, a map's key or value, or a channel.
func (w *walker) contains(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t := types.Unalias(t).(type) {
	case *types.Named:
		return w.isBox(t) || w.contains(t.Underlying(), seen)
	case *types.Pointer:
		return w.contains(t.Elem(), seen)
	case *types.Slice:
		return w.contains(t.Elem(), seen)
	case *types.Array:
		return w.contains(t.Elem(), seen)
	case *types.Map:
		return w.contains(t.Key(), seen) || w.contains(t.Elem(), seen)
	case *types.Chan:
		return w.contains(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if w.contains(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// external reports whether t reaches one of the inventory's external error types.
func (w *walker) external(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t := types.Unalias(t).(type) {
	case *types.Named:
		if t.Obj().Pkg() != nil {
			name := t.Obj().Pkg().Path() + "." + t.Obj().Name()
			for _, e := range w.g.inv.Gate.External {
				if e == name {
					return true
				}
			}
		}
		return w.external(t.Underlying(), seen)
	case *types.Pointer:
		return w.external(t.Elem(), seen)
	case *types.Slice:
		return w.external(t.Elem(), seen)
	case *types.Array:
		return w.external(t.Elem(), seen)
	case *types.Map:
		return w.external(t.Key(), seen) || w.external(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if w.external(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

func (w *walker) file(f *ast.File) {
	var stack []ast.Node
	function := "<package>"
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if fd, ok := n.(*ast.FuncDecl); ok {
			function = goFuncName(w.path, fd)
		}
		parents := stack
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.StructType:
			w.holder(n, parents, function)
		case *ast.Ident:
			w.instance(n, function)
		case *ast.SelectorExpr:
			w.storage(n, parents, function)
		case *ast.KeyValueExpr:
			w.storageWrite(n, function)
		case *ast.CallExpr:
			w.reflection(n, parents, function)
		}
		return true
	})
	if w.writes > 1 {
		file, line := w.at(f)
		w.c.Box = append(w.c.Box, finding{File: file, Line: line, What: w.g.inv.Gate.Box.Type + "." + w.g.inv.Gate.Box.Field, Kind: "write", Why: fmt.Sprintf("the storage is assigned %d times; the box has one assignment, in construction", w.writes)})
	}
}

// holder lists a struct type that holds the box.
func (w *walker) holder(n *ast.StructType, parents []ast.Node, function string) {
	tv, ok := w.info.Types[n]
	if !ok || !w.contains(tv.Type, map[types.Type]bool{}) {
		return
	}
	file, line := w.at(n)
	name := ""
	if len(parents) > 0 {
		if spec, ok := parents[len(parents)-1].(*ast.TypeSpec); ok {
			if obj := w.info.Defs[spec.Name]; obj != nil {
				name = obj.Pkg().Path() + "." + obj.Name()
			}
		}
	}
	if name == w.boxPkg+"."+w.boxName {
		return
	}
	if name == "" {
		// An anonymous struct is named by the function that declares it and its type, which a
		// print test in the same package can spell, so the name holds while lines move.
		name = function + " " + types.TypeString(tv.Type, func(p *types.Package) string { return p.Name() })
	}
	w.c.Holders = append(w.c.Holders, finding{File: file, Line: line, Function: function, What: name, Kind: "holder"})
}

// instance lists an instance of a generic type whose type arguments make it hold the box, such as
// listenerwire.APIErrorAs[redact.RemoteText]: the generic declaration's struct holds a type
// parameter where the instance holds the box, so holder, which reads declarations, cannot see it.
// It is listed once a package, by its full type (pkg/path.Name[pkg/path.Arg, ...]), which a print
// test can spell however many functions name the instance.
func (w *walker) instance(id *ast.Ident, function string) {
	inst, ok := w.info.Instances[id]
	if !ok {
		return
	}
	named, ok := inst.Type.(*types.Named)
	if !ok || !w.contains(named, map[types.Type]bool{}) {
		return
	}
	name := types.TypeString(named, nil)
	if w.instances[name] {
		return
	}
	w.instances[name] = true
	file, line := w.at(id)
	w.c.Holders = append(w.c.Holders, finding{File: file, Line: line, Function: function, What: name, Kind: "holder"})
}

// storageField reports whether sel selects the box's storage field.
func (w *walker) storageField(id *ast.Ident) bool {
	v, ok := w.info.Uses[id].(*types.Var)
	if !ok || !v.IsField() || v.Name() != w.g.inv.Gate.Box.Field || v.Pkg() == nil || v.Pkg().Path() != w.boxPkg {
		return false
	}
	box := v.Pkg().Scope().Lookup(w.boxName)
	if box == nil {
		return false
	}
	st, ok := box.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if st.Field(i) == v {
			return true
		}
	}
	return false
}

// storageWrite counts the box's construction: its storage set in a composite literal.
func (w *walker) storageWrite(n *ast.KeyValueExpr, function string) {
	id, ok := n.Key.(*ast.Ident)
	if !ok || !w.storageField(id) {
		return
	}
	w.writes++
	file, line := w.at(n)
	w.c.Box = append(w.c.Box, finding{File: file, Line: line, Function: function, What: w.boxName + "." + id.Name, Kind: "write", OK: true, Why: "the assignment in construction"})
}

// storage classifies one use of the box's storage by its immediate operation: the argument of a
// rule call, an operand of a pure comparison, a nil check, or the assignment in construction.
// Anything else fails, and so does any use outside the box's package.
func (w *walker) storage(sel *ast.SelectorExpr, parents []ast.Node, function string) {
	if !w.storageField(sel.Sel) {
		return
	}
	file, line := w.at(sel)
	f := finding{File: file, Line: line, Function: function, What: w.boxName + "." + sel.Sel.Name}
	defer func() { w.c.Box = append(w.c.Box, f) }()
	if w.path != w.boxPkg {
		f.Kind, f.Why = "outside", "a read of the box's storage outside package "+w.boxPkg
		return
	}
	up := func(i int) (ast.Node, int) {
		for ; i >= 0; i-- {
			if _, paren := parents[i].(*ast.ParenExpr); !paren {
				return parents[i], i
			}
		}
		return nil, -1
	}
	parent, i := up(len(parents) - 1)
	var child ast.Expr = sel
	switch p := parent.(type) {
	case *ast.BinaryExpr:
		if (p.Op == token.EQL || p.Op == token.NEQ) && (isNil(p.X) || isNil(p.Y)) {
			f.Kind, f.OK, f.Why = "nil-check", true, "the storage compared with nil"
			return
		}
	case *ast.AssignStmt:
		for _, lhs := range p.Lhs {
			if ast.Unparen(lhs) == sel {
				w.writes++
				f.Kind, f.OK, f.Why = "write", true, "the assignment in decoding"
				return
			}
		}
	case *ast.StarExpr:
		// The text behind the pointer, as the box holds it; a box that held it by value is read
		// without the star, and the same operations classify it.
		child = p
		parent, i = up(i - 1)
	}
	switch q := parent.(type) {
	case *ast.BinaryExpr:
		if q.Op == token.EQL || q.Op == token.NEQ {
			f.Kind, f.OK, f.Why = "comparison", true, "an operand of "+q.Op.String()
			return
		}
	case *ast.CallExpr:
		callee := w.callee(q)
		for _, arg := range q.Args {
			if ast.Unparen(arg) == child {
				switch {
				case slices.Contains(w.g.inv.Gate.Box.Rule, callee):
					f.Kind, f.OK, f.Why = "rule", true, "the argument of "+callee
					return
				case slices.Contains(w.g.inv.Gate.Box.Comparisons, callee):
					f.Kind, f.OK, f.Why = "comparison", true, "an operand of "+callee
					return
				case slices.Contains(w.g.inv.Gate.Box.Parses, callee):
					if why := w.parseErrorKept(q, parents[:i]); why != "" {
						f.Kind, f.Why = "raw", "the text is the argument of "+callee+", "+why
						return
					}
					f.Kind, f.OK, f.Why = "parse", true, "the argument of "+callee+", whose error, which quotes the text, is only compared with nil"
					return
				}
				f.Kind, f.Why = "raw", "the text is the argument of "+orName(callee, "a function value")+", which is no rule, no comparison and no parse"
				return
			}
		}
	}
	f.Kind, f.Why = "raw", "the text reaches "+describe(parent)
}

// parseErrorKept says how the parse call lets its error, which quotes the text it was given,
// escape, or answers "": the call must be the one value of an assignment whose last target is _
// or a local every use of which, in the function around it, is an operand of == or != with nil.
// above is the path from the file down to the call's parent.
func (w *walker) parseErrorKept(call *ast.CallExpr, above []ast.Node) string {
	var parent ast.Node
	for j := len(above) - 1; j >= 0; j-- {
		if _, paren := above[j].(*ast.ParenExpr); !paren {
			parent = above[j]
			break
		}
	}
	assign, ok := parent.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 || ast.Unparen(assign.Rhs[0]) != call || len(assign.Lhs) < 2 {
		return "whose error is not assigned apart"
	}
	id, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok {
		return "whose error is assigned to no local"
	}
	if id.Name == "_" {
		return ""
	}
	obj := w.info.ObjectOf(id)
	var body ast.Node
	for j := len(above) - 1; j >= 0 && body == nil; j-- {
		switch fn := above[j].(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		}
	}
	if obj == nil || body == nil {
		return "whose error cannot be followed"
	}
	why := ""
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		use, ok := n.(*ast.Ident)
		if !ok || use == id || w.info.Uses[use] != obj || why != "" {
			return true
		}
		var user ast.Node
		for j := len(stack) - 2; j >= 0; j-- {
			if _, paren := stack[j].(*ast.ParenExpr); !paren {
				user = stack[j]
				break
			}
		}
		if b, ok := user.(*ast.BinaryExpr); ok && (b.Op == token.EQL || b.Op == token.NEQ) && (isNil(b.X) || isNil(b.Y)) {
			return true
		}
		file, line := w.at(use)
		why = fmt.Sprintf("whose error, which quotes the text, reaches %s at %s:%d", describe(user), file, line)
		return true
	})
	return why
}

// callee is the full name of the function or method call calls (calleeFunc), its generic origin's
// for an instance, or "".
func (w *walker) callee(call *ast.CallExpr) string {
	if fn := calleeFunc(w.info, call); fn != nil {
		return fn.Origin().FullName()
	}
	return ""
}

func isNil(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && id.Name == "nil"
}

func orName(name, otherwise string) string {
	if name == "" {
		return otherwise
	}
	return name
}

func describe(n ast.Node) string {
	switch n.(type) {
	case nil:
		return "nothing the census knows"
	case *ast.ReturnStmt:
		return "a returned value"
	case *ast.BinaryExpr:
		return "a string built from it"
	case *ast.CompositeLit, *ast.KeyValueExpr:
		return "a composite value"
	case *ast.AssignStmt, *ast.ValueSpec:
		return "a variable"
	case *ast.IndexExpr, *ast.SliceExpr:
		return "an index or slice of it"
	case *ast.CallExpr:
		return "a call"
	}
	return fmt.Sprintf("a %T", n)
}

// reflectionMethods are reflect's struct-field access.
var reflectionMethods = map[string]bool{
	"(reflect.Value).Field":           true,
	"(reflect.Value).FieldByName":     true,
	"(reflect.Value).FieldByIndex":    true,
	"(reflect.Value).FieldByIndexErr": true,
	"(reflect.Value).FieldByNameFunc": true,
}

// reflection lists a call of reflect's struct-field access and an unsafe.Pointer conversion, in
// every package, the box's own among them, and allows one only where the inventory names its file
// and function and every type it reaches (its operands) holds no box storage and no external
// error type: the operand of a conversion is the converted value's type, and those of a reflect
// call the static types reflect.ValueOf is given in the package (through one level of an
// interface parameter). An entry that says its walk reads exported fields alone
// (exported_fields_only) is allowed instead where each of its calls is a reflect.Value.Field that
// runs only under a t.Field(i).IsExported() guard on its own index (exportedGuarded): the box
// keeps its text in unexported fields, so such a walk cannot reach it, whatever type it is given.
// An entry that names a guarded package P (guarded_package) is allowed instead for one shape of
// read alone: a reflect.Value.FieldByName with a constant name, on a variable that runs only under
// a guard holding its type to P (packageGuarded), whose result is only the receiver of methods that
// answer nothing or closed values - booleans, numbers other than uintptr, strings, and slices and
// arrays of them (closedValue) - or one variable's value used only so (resultReceiver, fieldRead).
// P's import closure must hold neither the box's package nor an external error type's, and P must
// declare no generic type (closureClean), so no type P declares can name the box. P's source is
// type-checked for the build being read (sourceChecked), every named type it declares at package
// level or in a function body among it, and in every struct type of those each field of the name
// that FieldByName can reach (fieldsNamed) - the struct's own, and those of every struct it embeds,
// by value or by pointer, from any package, at any depth, another package's read with its
// unexported fields from that package's export data, and one FieldByName does not return, shadowed
// by a shallower one or ambiguous at its depth, among them - must be typed with no interface and no
// type parameter at any depth (through a pointer, a slice, an array, a map, a channel or a struct's
// fields), and at least one struct type must reach such a field: only an interface or a type
// parameter could hold the box at run time. Not covered, so refused: any other reflect call,
// FieldByIndex and Field among them; a name that is no constant; a result passed on, stored twice,
// or the receiver of a method that answers a reflect.Value, a reflect.Type, an interface, a func, a
// pointer or a uintptr (Elem, Interface, Call, CallSlice, Method, MethodByName, Type, Index and
// Addr among them), each of which reaches past the field's own value. A func type counts as holding
// nothing and a type's methods are not read: a func's captured values are out of reflect's reach,
// and its results and a method's only such a method reaches.
func (w *walker) reflection(call *ast.CallExpr, parents []ast.Node, function string) {
	kind, what := "", ""
	if callee := w.callee(call); reflectionMethods[callee] {
		kind, what = "reflect", callee
	} else if tv, ok := w.info.Types[call.Fun]; ok && tv.IsType() && len(call.Args) == 1 {
		to := tv.Type
		from := w.info.Types[call.Args[0]].Type
		if isUnsafePointer(to) || isUnsafePointer(from) {
			kind, what = "unsafe", fmt.Sprintf("a conversion from %s to %s", typeString(from), typeString(to))
		}
	}
	if kind == "" {
		return
	}
	file, line := w.at(call)
	f := finding{File: file, Line: line, Function: function, What: what, Kind: kind}
	defer func() { w.c.Reflect = append(w.c.Reflect, f) }()
	var exempt *reflectionExempt
	for i := range w.g.inv.Gate.Reflection {
		e := &w.g.inv.Gate.Reflection[i]
		if e.File == file && e.Function == function {
			exempt = e
		}
	}
	if exempt == nil {
		f.Why = "no inventory entry names this site"
		return
	}
	// The box's own record walk (redact.recordBoxes) reaches every type a caller decodes an answer
	// into, box-holding ones by design: it hands each box its record through the box's own method
	// and reads no storage, which the box census holds, since that census allows the storage to be
	// read only by the box's rule and comparison methods. An entry may name such a walk only in the
	// box's own package; anywhere else a reflection that reaches the box fails as before.
	if exempt.RecordsBoxes {
		if w.path != w.boxPkg || kind != "reflect" {
			f.Why = "records_boxes names a reflection outside the box's package, or no reflect call"
			return
		}
		f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); the box's own walk, which hands each box its record and reads no storage"
		return
	}
	if exempt.ExportedFieldsOnly {
		switch {
		case ast.IsExported(w.g.inv.Gate.Box.Field):
			f.Why = "exported_fields_only names a walk of exported fields, and the box's storage field is exported"
		case what != "(reflect.Value).Field":
			f.Why = "exported_fields_only names a reflection other than reflect.Value.Field"
		case !w.exportedGuarded(call, parents):
			f.Why = "exported_fields_only names a walk whose Field here does not run only under a t.Field(i).IsExported() guard on its own index"
		default:
			f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); it reads an exported field alone, and the box keeps its text in unexported fields"
		}
		return
	}
	if exempt.GuardedPackage != "" {
		guarded, why := w.packageGuarded(call, parents)
		switch {
		case kind != "reflect":
			f.Why = "guarded_package names a conversion, which no type guard holds"
		case what != "(reflect.Value).FieldByName":
			f.Why = "guarded_package admits a reflect.Value.FieldByName with a constant name alone, and this is " + what
		case guarded == "":
			f.Why = "guarded_package names a reflection that does not run only under a guard holding its value's type to one package: " + why
		case guarded != exempt.GuardedPackage:
			f.Why = "guarded_package names " + exempt.GuardedPackage + ", and the guard here holds the value's type to " + guarded
		default:
			closure, cleanWhy := w.closureClean(guarded)
			if !closure {
				f.Why = cleanWhy
				break
			}
			read, readWhy := w.fieldRead(call, parents, guarded)
			if !read {
				f.Why = readWhy
				break
			}
			f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); "+cleanWhy+"; "+readWhy
		}
		return
	}
	var operands []types.Type
	if kind == "unsafe" {
		operands = append(operands, w.info.Types[call.Args[0]].Type)
	} else {
		operands = w.g.valueOfOperands(w.path)
		if operands == nil {
			f.Why = "the types this reflection reaches cannot be read: no reflect.ValueOf in the package gives it a concrete type"
			return
		}
	}
	var unlisted, boxed, external []string
	for _, t := range operands {
		switch {
		case w.contains(t, map[types.Type]bool{}):
			boxed = append(boxed, typeString(t))
		case w.external(t, map[types.Type]bool{}):
			external = append(external, typeString(t))
		case !w.listed(t, exempt.Operands):
			unlisted = append(unlisted, typeString(t))
		}
	}
	switch {
	case len(boxed) > 0:
		f.Why = "it reaches " + strings.Join(boxed, ", ") + ", which holds box storage"
	case len(external) > 0:
		f.Why = "it reaches " + strings.Join(external, ", ") + ", which holds an external error type"
	case len(unlisted) > 0:
		f.Why = "it reaches " + strings.Join(unlisted, ", ") + ", which its inventory entry does not list"
	default:
		f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); its operands hold no box storage and no external error type"
	}
}

// exportedGuarded reports whether call, a reflect.Value.Field whose index is an identifier, runs
// only where that field is exported: it is the right operand of an && whose left operand's chain
// of && holds t.Field(i).IsExported() on the same i (t a reflect.Type), or in an if's body whose
// condition's chain does, within the function the call is in.
func (w *walker) exportedGuarded(call *ast.CallExpr, parents []ast.Node) bool {
	if len(call.Args) != 1 {
		return false
	}
	index, ok := ast.Unparen(call.Args[0]).(*ast.Ident)
	if !ok || w.info.Uses[index] == nil {
		return false
	}
	child := ast.Node(call)
	for i := len(parents) - 1; i >= 0; i-- {
		switch p := parents[i].(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return false
		case *ast.BinaryExpr:
			if p.Op == token.LAND && p.Y == child && w.guards(p.X, w.info.Uses[index]) {
				return true
			}
		case *ast.IfStmt:
			if p.Body == child && w.guards(p.Cond, w.info.Uses[index]) {
				return true
			}
		}
		child = parents[i]
	}
	return false
}

// guards reports whether cond, read as a chain of &&, holds t.Field(i).IsExported() for the
// index i, t a reflect.Type.
func (w *walker) guards(cond ast.Expr, index types.Object) bool {
	cond = ast.Unparen(cond)
	if both, ok := cond.(*ast.BinaryExpr); ok && both.Op == token.LAND {
		return w.guards(both.X, index) || w.guards(both.Y, index)
	}
	exported, ok := cond.(*ast.CallExpr)
	if !ok || len(exported.Args) != 0 || w.callee(exported) != "(reflect.StructField).IsExported" {
		return false
	}
	sel, ok := ast.Unparen(exported.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	field, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || len(field.Args) != 1 || w.callee(field) != "(reflect.Type).Field" {
		return false
	}
	at, ok := ast.Unparen(field.Args[0]).(*ast.Ident)
	return ok && w.info.Uses[at] == index
}

// packageGuarded is the package a guard holds the type of call's value to, or "" and why there
// is none. call is a reflect.Value method whose receiver is a variable X of the function the call
// is in, which one statement of the package stores to, before the guard, and none takes the
// address of. The guard is a statement of that function's body, before the one holding call, of
// the form `if ... || X.Type().PkgPath() != P || ... { ...; return ... }`, with no init and no
// else, in a function with no label, so no goto passes it; and P is reflect.TypeFor[T]().PkgPath()
// for a named type T, written in place or as the one initializer of a package-level variable no
// other statement of the package stores to (pkgPathOf). The package is T's.
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
	var body *ast.BlockStmt
	for i := len(parents) - 1; i >= 0 && body == nil; i-- {
		switch fn := parents[i].(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		}
	}
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

// pkgPathOf is the package e names as reflect.TypeFor[T]().PkgPath() does, T a named type: e
// itself, or an unexported package-level variable whose one store is that initializer
// (collectStores); "" for any other expression. collectStores reads the stores of the guard's own
// package, and an exported variable can be stored to by any package that imports it, so an
// exported one names no package here.
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
// checked against (closureClean) is never narrower than the build being read.
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

// closureClean reports whether pkg's import closure (recordImports) holds neither the box's
// package nor the package of an external error type, and pkg declares no generic type, and says
// why. The closure stands for every type pkg declares only where those types are pkg's own: an
// instance of a generic type of pkg holds whatever its type argument is, the box among them,
// while reflect names pkg as its package (PkgPath) and pkg's import closure shows no box.
func (w *walker) closureClean(pkg string) (bool, string) {
	closure := w.g.imports[pkg]
	if closure == nil {
		return false, "guarded_package names " + pkg + ", whose import closure no go list of these builds names"
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
		return false, "guarded_package names " + pkg + ", whose import closure holds " + strings.Join(held, " and ")
	}
	declared := w.g.sourceChecked(pkg)
	if declared.err != "" {
		return false, "guarded_package names " + pkg + ", whose source cannot be type-checked: " + declared.err
	}
	var generic []string
	for _, tn := range declared.declaredTypes() {
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
	if len(generic) > 0 {
		return false, "guarded_package names " + pkg + ", which declares the generic type " + strings.Join(generic, ", ") + ": an instance is " + pkg + "'s to reflect and holds whatever its type argument is, the box among them, which no import closure shows"
	}
	return true, "its guard holds its value's type to " + pkg + ", whose import closure holds neither the box's package nor an external error type's, and which declares no generic type"
}

// fieldRead reports whether call, a guarded reflect.Value.FieldByName on a value of a type of pkg,
// is the one read guarded_package admits, and says why: a constant name, a result that is only the
// receiver of methods that answer closed values (resultReceiver), and, in every struct type pkg
// declares, every field of that name FieldByName can reach (fieldsNamed: the struct's own, and those
// of each struct it embeds, from any package, by value or by pointer, at any depth) typed with no
// interface and no type parameter at any depth (holdsDynamic), with at least one struct type that
// reaches such a field.
func (w *walker) fieldRead(call *ast.CallExpr, parents []ast.Node, pkg string) (bool, string) {
	tv := w.info.Types[call.Args[0]]
	if tv.Value == nil || tv.Value.Kind() != constant.String {
		return false, "guarded_package admits a FieldByName whose name is a constant, and this one's is not"
	}
	name := constant.StringVal(tv.Value)
	if why := w.resultReceiver(call, parents); why != "" {
		return false, "guarded_package admits a FieldByName whose result is only the receiver of methods that answer nothing or closed values (booleans, numbers other than uintptr, strings, and slices and arrays of them): " + why
	}
	declared := w.g.sourceChecked(pkg)
	if declared.err != "" {
		return false, "guarded_package names " + pkg + ", whose source cannot be type-checked: " + declared.err
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
	return true, fmt.Sprintf("FieldByName(%q) reaches no interface and no type parameter in any of the %d struct types %s declares that hold a field of that name, promoted ones from any package at any depth included, and its result is only the receiver of methods that answer closed values", name, found, pkg)
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

// resultReceiver is "" where call's result is only the receiver of methods that answer nothing or
// closed values (closedValue), or the value of one variable stored once whose every use is such a
// receiver; else why not. A method that answers a reflect.Value, a reflect.Type, an interface, a
// func, a pointer or a uintptr reaches past the field's own value: Elem and Interface what an
// interface or a pointer holds; Call and CallSlice a func field's results; Method and MethodByName
// a method's, and Type the same through its Method(i).Func; Index, MapIndex, MapKeys, MapRange,
// Seq, Seq2, Recv and Slice an element; Addr and Convert another view of the field, whose own
// methods the census would not read; Pointer, UnsafeAddr and UnsafePointer an address. So each is
// refused, and the field's value is read only as a closed value.
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
	var body ast.Node
	for i := len(parents) - 1; i >= 0 && body == nil; i-- {
		switch fn := parents[i].(type) {
		case *ast.FuncDecl:
			body = fn.Body
		case *ast.FuncLit:
			body = fn.Body
		}
	}
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
// parameter. A func type counts as holding none, and a type's methods are not read: a func's
// captured values are out of reflect's reach, and its results and a method's are reached from a
// guarded read's result only by a method that answers no closed value (Call, CallSlice, Method,
// MethodByName, Type and Interface among them), which resultReceiver refuses.
func holdsDynamic(t types.Type, seen map[types.Type]bool) bool {
	t = types.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Interface, *types.TypeParam:
		return true
	case *types.Named:
		return holdsDynamic(t.Underlying(), seen)
	case *types.Pointer:
		return holdsDynamic(t.Elem(), seen)
	case *types.Slice:
		return holdsDynamic(t.Elem(), seen)
	case *types.Array:
		return holdsDynamic(t.Elem(), seen)
	case *types.Chan:
		return holdsDynamic(t.Elem(), seen)
	case *types.Map:
		return holdsDynamic(t.Key(), seen) || holdsDynamic(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if holdsDynamic(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// closedValue reports whether a value of t is closed: it holds no value of another type and no
// address, so a method of a guarded read's result that answers only closed values reaches nothing
// past the field (resultReceiver). A boolean, a number other than a uintptr, a string, a defined
// type over one (reflect.Kind), and a slice or an array of closed values are; a reflect.Value, an
// interface (reflect.Type among them), a func, a pointer, a map, a channel, a struct, a uintptr and
// an unsafe.Pointer are not.
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

// goSource is a listed package's directory and files, and the export data of every package the
// build it was listed in compiles (recordSource): what type-checks it from source.
type goSource struct {
	dir     string
	files   []string
	cgo     []string
	exports map[string]string
}

// checkedSource is a package type-checked from its source (sourceChecked), or why it could not be.
type checkedSource struct {
	pkg   *types.Package
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

// recordSource keeps where p's source is and the export data of the build that listed it, so a
// guarded package can be type-checked from source for that build (sourceChecked).
func (g *gate) recordSource(p goListed, exports map[string]string) {
	if g.sources == nil {
		g.sources = map[string]goSource{}
	}
	g.sources[p.ImportPath] = goSource{dir: p.Dir, files: p.GoFiles, cgo: p.CgoFiles, exports: exports}
}

// sourceChecked is pkg type-checked from its source files for the build its listing last read
// (recordSource), its imports from that build's export data; cached by package and build. Export
// data holds an unexported type only where an exported declaration names it, so the guarded
// package's own types are read from its source.
func (g *gate) sourceChecked(pkg string) *checkedSource {
	key := pkg
	src, listed := g.sources[pkg]
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
		checked.err = "no go list of these builds names its source"
		return checked
	case len(src.cgo) > 0:
		checked.err = "it has cgo files (" + strings.Join(src.cgo, ", ") + "), which the census does not read"
		return checked
	}
	fset := token.NewFileSet()
	for _, name := range src.files {
		f, err := parser.ParseFile(fset, filepath.Join(src.dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			checked.err = err.Error()
			return checked
		}
		checked.files = append(checked.files, f)
	}
	checked.info = &types.Info{Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		if e := src.exports[p]; e != "" {
			return os.Open(e)
		}
		return nil, fmt.Errorf("no export data for %s", p)
	})}
	typed, err := conf.Check(pkg, fset, checked.files, checked.info)
	if err != nil {
		checked.err = err.Error()
		return checked
	}
	checked.pkg = typed
	return checked
}

func (w *walker) listed(t types.Type, operands []string) bool {
	s := typeString(t)
	for _, o := range operands {
		if o == s {
			return true
		}
	}
	return false
}

func isUnsafePointer(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.UnsafePointer
}

func typeString(t types.Type) string {
	if t == nil {
		return "<untyped>"
	}
	return types.TypeString(t, nil)
}

// valueOfOperands is the static types reflect.ValueOf is given in the package at path, each
// interface-typed argument that is a parameter of the enclosing function replaced by the static
// types its callers in the package pass for it; nil when one cannot be resolved.
func (g *gate) valueOfOperands(path string) []types.Type {
	sites := g.valueOf[path]
	if len(sites) == 0 {
		return nil
	}
	var out []types.Type
	seen := map[string]bool{}
	for _, s := range sites {
		resolved := s.resolved
		if resolved == nil {
			return nil
		}
		for _, t := range resolved {
			if !seen[typeString(t)] {
				seen[typeString(t)] = true
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return typeString(out[i]) < typeString(out[j]) })
	return out
}
