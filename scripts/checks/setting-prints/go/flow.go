//go:build ignore

package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

func (a *analysis) ev(e ast.Expr) set {
	if e == nil {
		return nil
	}
	if v, ok := a.memo[e]; ok {
		return v
	}
	a.memo[e] = nil
	v := a.evaluate(e)
	if len(v) > 0 && inert(a.info.TypeOf(e)) {
		kept := set{}
		if _, fn := a.info.TypeOf(e).Underlying().(*types.Signature); fn {
			for id := range v {
				if k := a.labels[id].kind; k == "function" || k == "environment function" {
					kept[id] = true
				}
			}
		}
		v = kept
	}
	a.memo[e] = v
	return v
}

func (a *analysis) funcValue(o types.Object) set {
	fn, ok := o.(*types.Func)
	if !ok {
		return nil
	}
	if packagePath(fn) == "os" && (fn.Name() == "Getenv" || fn.Name() == "LookupEnv") {
		return a.fnLabel("environment function", fn)
	}
	if a.bodies[fn] != nil {
		return a.fnLabel("function", fn)
	}
	return nil
}

func (a *analysis) evaluate(e ast.Expr) set {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return a.ev(x.X)
	case *ast.Ident:
		o := a.object(x)
		if v, ok := o.(*types.Var); ok {
			if local(v) {
				return a.reaching(v, x.Pos())
			}
			return a.carried[types.Object(v)]
		}
		return a.funcValue(o)
	case *ast.SelectorExpr:
		if sel := a.info.Selections[x]; sel != nil {
			if sel.Kind() == types.FieldVal {
				// A field of a struct this program reads is followed by itself: every write of it is
				// read. One of a value a function it does not read made holds what that value holds.
				if a.ours(a.info.TypeOf(x.X)) {
					return a.carried[origin(sel.Obj())]
				}
				return union(a.carried[origin(sel.Obj())], a.ev(x.X))
			}
			return a.funcValue(origin(sel.Obj()))
		}
		o := a.object(x.Sel)
		if packagePath(o) == "os" && o.Name() == "Args" {
			return a.source(x, "os.Args")
		}
		if v, ok := o.(*types.Var); ok {
			return a.carried[types.Object(v)]
		}
		return a.funcValue(o)
	case *ast.CallExpr:
		return a.evaluateCall(x)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return union(a.write(x.X, "a string +", a.ev(x.X), true), a.write(x.Y, "a string +", a.ev(x.Y), false))
		}
		return nil
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return a.ev(x.X)
		}
		return nil
	case *ast.StarExpr:
		return a.ev(x.X)
	case *ast.IndexExpr:
		return a.ev(x.X)
	case *ast.IndexListExpr:
		return a.ev(x.X)
	case *ast.SliceExpr:
		return a.ev(x.X)
	case *ast.TypeAssertExpr:
		return a.ev(x.X)
	case *ast.KeyValueExpr:
		return a.ev(x.Value)
	case *ast.CompositeLit:
		all := []set{a.typeLabel(a.info.TypeOf(x))}
		for _, el := range x.Elts {
			all = append(all, a.ev(el))
		}
		return union(all...)
	case *ast.FuncLit:
		return a.fnLabel("function", x)
	}
	return nil
}

// variable is a variable of the environment, the same label wherever it is read.
func (a *analysis) variable(name string) int {
	return a.intern("v "+name, func() *label {
		return &label{kind: "source", what: "the environment variable " + name}
	})
}

// named is value with the environment as a whole in it named by the constant call is handed that
// is spelled as a variable of the environment, when it is handed one: the variable of that name.
func (a *analysis) named(call *ast.CallExpr, value set) set {
	key := ""
	for _, arg := range call.Args {
		if tv, ok := a.info.Types[arg]; ok && tv.Value != nil && tv.Value.Kind() == constant.String && envName.MatchString(constant.StringVal(tv.Value)) {
			key = constant.StringVal(tv.Value)
			break
		}
	}
	if key == "" {
		return value
	}
	var out set
	for id := range value {
		if l := a.labels[id]; l.kind == "source" && l.keyed {
			if out == nil {
				out = set{}
				for each := range value {
					out[each] = true
				}
			}
			delete(out, id)
			out[a.variable(key)] = true
		}
	}
	if out == nil {
		return value
	}
	return out
}

// environment is the environment as a whole, read at n.
func (a *analysis) environment(n ast.Node, what string) set {
	file, line := a.position(n)
	where := fmt.Sprintf("%s:%d", file, line)
	return set{a.intern("e "+where+" "+what, func() *label {
		return &label{kind: "source", keyed: true, what: what + " (" + where + ")"}
	}): true}
}

func (a *analysis) evaluateCall(call *ast.CallExpr) set {
	if tv, ok := a.info.Types[call.Fun]; ok && tv.IsType() {
		if len(call.Args) == 1 {
			return a.ev(call.Args[0])
		}
		return nil
	}
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok {
		if b, ok := a.info.Uses[id].(*types.Builtin); ok {
			if b.Name() == "append" || b.Name() == "min" || b.Name() == "max" {
				var all []set
				for _, arg := range call.Args {
					all = append(all, a.ev(arg))
				}
				return union(all...)
			}
			if b.Name() == "new" && len(call.Args) == 1 {
				return a.typeLabel(a.info.TypeOf(call.Args[0]))
			}
			return nil
		}
	}
	fn := a.calleeOf(call)
	if fn != nil {
		if sanitizer(fn) {
			return nil
		}
		name := packagePath(fn) + "." + fn.Name()
		switch name {
		case "os.Getenv", "os.LookupEnv", "syscall.Getenv", "os.Environ":
			return a.named(call, a.environment(call, "the environment"))
		case "os.ReadFile", "os.Open", "os.OpenFile":
			return a.source(call, "the text of a file ("+a.exprText(call)+")")
		}
		if packagePath(fn) == "flag" {
			return a.source(call, "a flag ("+a.exprText(call)+")")
		}
		if a.bodies[fn] != nil {
			return a.named(call, a.answer(fn, call))
		}
	} else {
		var through []set
		for id := range a.ev(call.Fun) {
			switch l := a.labels[id]; l.kind {
			case "function":
				through = append(through, a.answer(l.fn, call))
			case "environment function":
				through = append(through, a.environment(call, "the environment, through "+a.exprText(call.Fun)))
			}
		}
		if len(through) > 0 {
			return a.named(call, union(through...))
		}
	}
	// A function outside the module, of a package whose answers and errors hold the text they are
	// handed (keepsText), hands on the text of each argument and receiver that can hold text
	// (texty). A method of an interface the module declares (abstract) answers what each
	// implementation the unit holds answers for this call, and hands on the text of each argument,
	// for an implementation outside the module; never its receiver's, a value whose fields this
	// program reads apart. Any function hands on what each function it is handed answers, and,
	// handed one, reads the setting a constant names.
	keeps := fn != nil && keepsText[packagePath(fn)]
	abstract := fn != nil && a.abstract(fn)
	var all []set
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && keeps && a.info.Selections[sel] != nil && a.texty(a.info.TypeOf(sel.X)) {
		all = append(all, a.ev(sel.X))
	}
	if abstract {
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			for _, impl := range a.implementations(fn, a.ev(sel.X)) {
				all = append(all, a.named(call, a.answer(impl, call)))
			}
		}
	}
	handed := false
	for _, arg := range call.Args {
		value := a.ev(arg)
		if (keeps || abstract) && a.texty(a.info.TypeOf(arg)) {
			all = append(all, value)
		}
		for id := range value {
			switch l := a.labels[id]; l.kind {
			case "function":
				all = append(all, a.answer(l.fn, nil))
				handed = true
			case "environment function":
				all = append(all, a.environment(arg, "the environment, through "+a.exprText(arg)))
				handed = true
			}
		}
	}
	value := union(all...)
	if handed {
		value = a.named(call, value)
	}
	if fn != nil && renders[packagePath(fn)+"."+fn.Name()] {
		// The text it makes holds each argument as written into it.
		var written []set
		for _, arg := range call.Args {
			if a.texty(a.info.TypeOf(arg)) {
				written = append(written, a.write(arg, packagePath(fn)+"."+fn.Name(), a.ev(arg), false))
			}
		}
		value = union(written...)
	}
	return value
}

// answer is what the call of fn (a *types.Func or an *ast.FuncLit) answers: its summary, each
// of its parameters filled in by what this call hands it, and each line of it that wrote one in
// the answer where this call hands that parameter a setting (writtenAt). For a call this program
// cannot name (nil), and for a parameter of another function the concrete reading meets in a
// summary (a function literal's use of its enclosing function's parameter), a parameter holds what
// every call hands it.
func (a *analysis) answer(fn any, call *ast.CallExpr) set {
	summary := a.returns[fn]
	if len(summary) == 0 {
		return nil
	}
	var all []set
	for id := range summary {
		l := a.labels[id]
		if l.kind != "param" {
			all = append(all, set{id: true})
			continue
		}
		var value set
		switch {
		case l.fn == fn && call != nil:
			value = a.argument(l, call)
		case l.fn == fn || !a.symbolic:
			value = a.carried[types.Object(l.param)]
		default:
			all = append(all, set{id: true})
			continue
		}
		if l.written {
			value = a.writtenAt(l.fn, l.index, value)
		}
		all = append(all, value)
	}
	return union(all...)
}

// argument is what call hands the parameter l of the function it runs: the receiver of a method
// call, the argument at its place, or every argument from the place of a variadic parameter on.
// A method value's receiver was bound where the value was made, so its call reads what that
// receiver holds.
func (a *analysis) argument(l *label, call *ast.CallExpr) set {
	if l.index < 0 {
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && a.info.Selections[sel] != nil && a.info.Selections[sel].Kind() == types.MethodVal {
			return a.ev(sel.X)
		}
		return a.carried[types.Object(l.param)]
	}
	var sig *types.Signature
	switch f := l.fn.(type) {
	case *types.Func:
		sig, _ = f.Type().(*types.Signature)
	case *ast.FuncLit:
		sig, _ = a.info.TypeOf(f).(*types.Signature)
	}
	if sig == nil || l.index >= len(call.Args) {
		return nil
	}
	if sig.Variadic() && l.index == sig.Params().Len()-1 && !call.Ellipsis.IsValid() {
		var all []set
		for _, arg := range call.Args[l.index:] {
			all = append(all, a.ev(arg))
		}
		return union(all...)
	}
	return a.ev(call.Args[l.index])
}

// reaching is what the variable v holds where it is read at at: what each assignment of it that
// can reach that point put there, from the latest assignment before it back to one that stands
// in a block holding at, which every path to at passes; an assignment in a block that does not
// hold at (a branch) is one path, so the search reads on past it. A read inside a function literal
// that v was not declared in can run after any assignment, so it reads them all.
func (a *analysis) reaching(v *types.Var, at token.Pos) set {
	held, siteMap := a.carried, a.sites
	if a.symbolic {
		held, siteMap = a.symCarried, a.symSites
	}
	sites := siteMap[v]
	if len(sites) == 0 {
		return held[types.Object(v)]
	}
	all := []set{held[types.Object(v)]}
	if lit := innermost(a.lits, at); lit != (span{}) && !lit.holds(v.Pos()) {
		for _, s := range sites {
			all = append(all, s.labels)
		}
		return union(all...)
	}
	var before []token.Pos
	for pos := range sites {
		if pos < at {
			before = append(before, pos)
		}
	}
	sort.Slice(before, func(i, j int) bool { return before[i] > before[j] })
	for _, pos := range before {
		s := sites[pos]
		all = append(all, s.labels)
		if s.block.holds(at) {
			return union(all[1:]...)
		}
	}
	return union(all...)
}

// assign puts value into the place target names: a variable (for a variable of a function, at
// this assignment), a field (and the value that holds it), an element of a map or a slice, or
// what a pointer points at. A field and a variable of a package hold what the concrete reading
// writes, never a parameter of the symbolic one.
func (a *analysis) assign(target ast.Expr, value set) {
	switch t := ast.Unparen(target).(type) {
	case *ast.Ident:
		if v, ok := a.object(t).(*types.Var); ok {
			if !local(v) {
				if !a.symbolic {
					a.grow(types.Object(v), value)
				}
				break
			}
			siteMap := a.sites
			if a.symbolic {
				siteMap = a.symSites
			}
			if siteMap[v] == nil {
				siteMap[v] = map[token.Pos]*site{}
			}
			s := siteMap[v][t.Pos()]
			if s == nil {
				s = &site{block: innermost(a.blocks, t.Pos()), labels: set{}}
				siteMap[v][t.Pos()] = s
			}
			for id := range value {
				if !s.labels[id] {
					s.labels[id] = true
					a.changed = true
				}
			}
		}
	case *ast.SelectorExpr:
		if sel := a.info.Selections[t]; sel != nil && sel.Kind() == types.FieldVal {
			if !a.symbolic {
				a.grow(origin(sel.Obj()), value)
			}
			a.assign(t.X, value)
		} else if v, ok := a.object(t.Sel).(*types.Var); ok && !a.symbolic {
			a.grow(types.Object(v), value)
		}
	case *ast.IndexExpr:
		a.assign(t.X, value)
	case *ast.StarExpr:
		a.assign(t.X, value)
	case *ast.UnaryExpr:
		if t.Op == token.AND {
			a.assign(t.X, value)
		}
	}
}

// bindCall puts each argument into its parameter of the function the call runs, and the receiver
// into the method's receiver: the concrete reading's parameter holds what every call hands it.
func (a *analysis) bindCall(call *ast.CallExpr, sig *types.Signature, receiver ast.Expr) {
	if sig == nil || a.symbolic {
		return
	}
	if receiver != nil && sig.Recv() != nil {
		a.grow(types.Object(sig.Recv()), a.ev(receiver))
	}
	params := sig.Params()
	for i, arg := range call.Args {
		if params.Len() == 0 {
			break
		}
		index := i
		if index >= params.Len() {
			index = params.Len() - 1
		}
		a.grow(types.Object(params.At(index)), a.ev(arg))
	}
}

// end is a place that prints arg: a line for the setting it holds as read, and an end for each line
// that wrote one into the text it holds.
func (a *analysis) end(arg ast.Expr, kind string, site ast.Node) {
	value := a.write(arg, kind, a.ev(arg), false)
	file, line := a.position(site)
	for id := range value {
		if l := a.labels[id]; (l.kind == "written" && len(l.sources) > 0) || l.kind == "chain" {
			l.ends[fmt.Sprintf("%s at %s:%d", kind, file, line)] = true
		}
	}
}

func (a *analysis) flowCall(call *ast.CallExpr) {
	fn := a.calleeOf(call)
	var receiver ast.Expr
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && a.info.Selections[sel] != nil {
		receiver = sel.X
	}
	if fn != nil && a.bodies[fn] != nil {
		sig, _ := fn.Type().(*types.Signature)
		a.bindCall(call, sig, receiver)
	} else if fn != nil && a.abstract(fn) && receiver != nil {
		for _, impl := range a.implementations(fn, a.ev(receiver)) {
			sig, _ := impl.Type().(*types.Signature)
			a.bindCall(call, sig, receiver)
		}
	} else if fn == nil {
		for id := range a.ev(call.Fun) {
			if l := a.labels[id]; l.kind == "function" {
				var sig *types.Signature
				switch f := l.fn.(type) {
				case *types.Func:
					sig, _ = f.Type().(*types.Signature)
				case *ast.FuncLit:
					sig, _ = a.info.TypeOf(f).(*types.Signature)
				}
				a.bindCall(call, sig, nil)
			}
		}
	} else if packagePath(fn) == "flag" && strings.HasSuffix(fn.Name(), "Var") && len(call.Args) > 0 {
		a.assign(call.Args[0], a.source(call, "a flag ("+a.exprText(call)+")"))
	} else if !sanitizer(fn) && keepsText[packagePath(fn)] {
		// A function of those packages writes the text it is handed into what a pointer it is given
		// points at (json.Unmarshal), and a writer method writes it into its receiver.
		var all []set
		for _, arg := range call.Args {
			if a.texty(a.info.TypeOf(arg)) {
				all = append(all, a.ev(arg))
			}
		}
		value := union(all...)
		if receiver != nil {
			if a.texty(a.info.TypeOf(receiver)) {
				value = union(value, a.ev(receiver))
			}
			switch fn.Name() {
			case "WriteString", "Write", "WriteByte", "WriteRune", "Set", "Add", "Store", "Grow":
				a.assign(receiver, value)
			}
		}
		for _, arg := range call.Args {
			if u, ok := ast.Unparen(arg).(*ast.UnaryExpr); ok && u.Op == token.AND {
				a.assign(u.X, value)
			}
		}
	}
	// A setting can be a URL when its value reaches a function that reads it as one (the URL it is
	// handed, or the head of its text, the base a + puts a path after), redact (the code's own
	// judgement that it is one), a child's command line, or a method of an interface the module
	// declares, whose implementation may use any text it is handed: the concrete reading says which.
	if !a.symbolic && fn != nil && a.bodies[fn] == nil {
		takes := urlTakers[packagePath(fn)+"."+fn.Name()] || sanitizer(fn)
		if takes || a.abstract(fn) {
			for _, arg := range call.Args {
				ids := a.settingsIn(a.ev(arg))
				if takes {
					ids = a.leadingIn(a.ev(arg))
				}
				for _, id := range ids {
					a.urls[id] = true
				}
			}
		}
	}
	if a.collect {
		if kind, args := a.sinkOf(call); kind != "" {
			for _, arg := range args {
				a.end(arg, kind, call)
			}
		}
	}
}

func (a *analysis) flowFile(file *ast.File) {
	var stack []ast.Node
	enclosing := func() any {
		for i := len(stack) - 1; i >= 0; i-- {
			switch f := stack[i].(type) {
			case *ast.FuncDecl:
				if fn, ok := a.info.Defs[f.Name].(*types.Func); ok {
					return fn
				}
				return nil
			case *ast.FuncLit:
				return f
			}
		}
		return nil
	}
	results := func(key any) *types.Tuple {
		switch f := key.(type) {
		case *types.Func:
			return f.Type().(*types.Signature).Results()
		case *ast.FuncLit:
			if sig, ok := a.info.TypeOf(f).(*types.Signature); ok {
				return sig.Results()
			}
		}
		return nil
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i := range x.Lhs {
					value := a.ev(x.Rhs[i])
					if x.Tok == token.ADD_ASSIGN {
						value = union(a.ev(x.Lhs[i]), a.write(x.Rhs[i], "a string +=", value, false))
					}
					a.assign(x.Lhs[i], value)
				}
			} else if len(x.Rhs) == 1 {
				for _, lhs := range x.Lhs {
					a.assign(lhs, a.ev(x.Rhs[0]))
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) == len(x.Values) {
				for i := range x.Names {
					a.assign(x.Names[i], a.ev(x.Values[i]))
				}
			} else if len(x.Values) == 1 {
				for _, name := range x.Names {
					a.assign(name, a.ev(x.Values[0]))
				}
			}
		case *ast.RangeStmt:
			value := a.ev(x.X)
			// The key of a map is what it is indexed by, which holds none of its values: a
			// settings map's keys are the variables' names. A slice's or a string's key is a number.
			isMap := false
			if t := a.info.TypeOf(x.X); t != nil {
				_, isMap = t.Underlying().(*types.Map)
			}
			if x.Key != nil && !isMap {
				a.assign(x.Key, value)
			}
			if x.Value != nil {
				a.assign(x.Value, value)
			}
		case *ast.ReturnStmt:
			// The symbolic reading's answer is the function's summary; the concrete reading reads
			// the results only for the lines that write a parameter into them.
			key := enclosing()
			var all []set
			for _, r := range x.Results {
				all = append(all, a.ev(r))
			}
			if tuple := results(key); tuple != nil {
				for i := range tuple.Len() {
					if tuple.At(i).Name() != "" {
						all = append(all, a.reaching(tuple.At(i), x.Pos()))
					}
				}
			}
			if a.symbolic {
				a.growIn(a.returns, key, union(all...))
			}
		case *ast.CompositeLit:
			if a.symbolic {
				break
			}
			t := a.info.TypeOf(x)
			if t == nil {
				break
			}
			if p, ok := t.Underlying().(*types.Pointer); ok {
				t = p.Elem()
			}
			st, ok := t.Underlying().(*types.Struct)
			if !ok {
				break
			}
			for i, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						if field, ok := a.object(id).(*types.Var); ok {
							a.grow(types.Object(field), a.ev(kv.Value))
						}
					}
				} else if i < st.NumFields() {
					a.grow(origin(st.Field(i)), a.ev(el))
				}
			}
		case *ast.SelectorExpr:
			// A method value takes its receiver with it, as the concrete reading reads it.
			if sel := a.info.Selections[x]; sel != nil && sel.Kind() == types.MethodVal && !a.symbolic {
				if parent, ok := stack[len(stack)-2].(*ast.CallExpr); !ok || parent.Fun != x {
					if fn, ok := origin(sel.Obj()).(*types.Func); ok && a.bodies[fn] != nil {
						if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
							a.grow(types.Object(sig.Recv()), a.ev(x.X))
						}
					}
				}
			}
		case *ast.CallExpr:
			a.flowCall(x)
		}
		return true
	})
}

// prepare binds each parameter of each function of files, and each method's receiver, to itself
// for the symbolic reading.
func (a *analysis) prepare(files []*ast.File) {
	bind := func(fn any, sig *types.Signature) {
		if sig == nil {
			return
		}
		if recv := sig.Recv(); recv != nil {
			a.growIn(a.symCarried, types.Object(recv), set{a.paramLabel(fn, -1, recv, false): true})
		}
		for i := range sig.Params().Len() {
			p := sig.Params().At(i)
			a.growIn(a.symCarried, types.Object(p), set{a.paramLabel(fn, i, p, false): true})
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if fn, ok := a.info.Defs[x.Name].(*types.Func); ok && a.bodies[fn] != nil {
					sig, _ := fn.Type().(*types.Signature)
					bind(fn, sig)
				}
			case *ast.FuncLit:
				sig, _ := a.info.TypeOf(x).(*types.Signature)
				bind(x, sig)
			}
			return true
		})
	}
}

// run reads one unit's files to a fixed point of both readings, then once more in the concrete
// reading collecting each line that prints. It answers the rounds.
func (a *analysis) run(files []*ast.File) int {
	a.reset(files)
	a.prepare(files)
	rounds := 0
	for {
		a.changed = false
		for _, symbolic := range []bool{true, false} {
			a.symbolic = symbolic
			a.memo = map[ast.Expr]set{}
			for _, f := range files {
				a.flowFile(f)
			}
		}
		rounds++
		if !a.changed || rounds >= 100 {
			break
		}
	}
	if a.changed {
		fail(fmt.Errorf("no fixed point in %d rounds", rounds))
	}
	a.symbolic = false
	a.collect = true
	a.memo = map[ast.Expr]set{}
	for _, f := range files {
		a.flowFile(f)
	}
	return rounds
}

// sinks counts the calls that print in files.
func (a *analysis) sinks(files []*ast.File) int {
	n := 0
	for _, f := range files {
		ast.Inspect(f, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if kind, _ := a.sinkOf(call); kind != "" {
					n++
				}
			}
			return true
		})
	}
	return n
}
