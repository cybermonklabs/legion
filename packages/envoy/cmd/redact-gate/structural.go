package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// structural runs the box, holder and reflection censuses over every production file of one
// type-checked package, read as build lists it.
func (g *gate) structural(c *census, dir, path string, build goBuild, fset *token.FileSet, files []*ast.File, info *types.Info) {
	box := g.inv.Gate.Box
	boxPkg, boxName := splitTypeName(box.Type)
	g.collectValueOf(path, files, info)
	stores, inits := collectStores(files, info)
	// The generic instances already listed in this package, each listed once.
	instances := map[string]bool{}
	for _, f := range files {
		rel, _ := filepath.Rel(dir, fset.Position(f.Pos()).Filename)
		rel = filepath.ToSlash(rel)
		w := &walker{g: g, c: c, fset: fset, info: info, rel: rel, path: path, build: build, boxPkg: boxPkg, boxName: boxName, instances: instances, stores: stores, inits: inits}
		w.file(f)
	}
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
	build           goBuild // the build the package is read as (sourceChecked)
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

// contains reports whether a value of type t holds the box anywhere (holdsBox).
func (w *walker) contains(t types.Type, seen map[types.Type]bool) bool {
	return holdsBox(t, w.boxPkg, w.boxName, seen)
}

// external reports whether t reaches one of the inventory's external error types, through every
// edge holdsBox follows, a channel's among them: a channel holds what is sent on it, and a
// reflection that reaches the channel's value reaches that.
func (w *walker) external(t types.Type, seen map[types.Type]bool) bool {
	return edges{chans: true}.reaches(t, func(t types.Type) bool {
		n, ok := t.(*types.Named)
		return ok && n.Obj().Pkg() != nil && slices.Contains(w.g.inv.Gate.External, n.Obj().Pkg().Path()+"."+n.Obj().Name())
	}, seen)
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
	body := enclosingBody(above)
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

// reflectionMethods are reflect's struct-field access: each method of reflect.Value that answers a
// struct's field, Fields, go 1.27's iterator over them, among them.
var reflectionMethods = map[string]bool{
	"(reflect.Value).Field":           true,
	"(reflect.Value).Fields":          true,
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
// An entry that names a guarded package (guarded_package) is allowed instead for the one read
// guardedPackageRead (guarded.go) states.
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
	mode, err := exempt.mode()
	if err != nil {
		// load refuses such an entry; a census given one unloaded passes no site by it.
		f.Why = "its inventory entry is refused: " + err.Error()
		return
	}
	switch mode {
	case byRecordsBoxes:
		// The box's own record walk (redact.recordBoxes) reaches every type a caller decodes an
		// answer into, box-holding ones by design: it hands each box its record through the box's
		// own method and reads no storage, which the box census holds, since that census allows the
		// storage to be read only by the box's rule and comparison methods. An entry may name such
		// a walk only in the box's own package; anywhere else a reflection that reaches the box
		// fails as before.
		if w.path != w.boxPkg || kind != "reflect" {
			f.Why = "records_boxes names a reflection outside the box's package, or no reflect call"
			return
		}
		f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); the box's own walk, which hands each box its record and reads no storage"
		return
	case byExportedFieldsOnly:
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
	case byGuardedPackage:
		if ok, why := w.guardedPackageRead(call, parents, kind, what, exempt.GuardedPackage); ok {
			f.OK, f.Why = true, "named in the inventory ("+exempt.Why+"); "+why
		} else {
			f.Why = why
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
