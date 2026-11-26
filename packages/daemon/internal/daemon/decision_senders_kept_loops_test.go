package daemon

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// This file holds the rule for a list of kept decisions (record.ParkedDecision and
// record.DecisionMark, keptDecisionTypes) that
// TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord holds the three sender packages to,
// the loops it allows (keptLoopReaders) and the test of that list's rows;
// decision_senders_kept_rows_test.go plants each shape the rule must read.
//
// The rule has two halves.
//
// A loop, a for or a range over anything (a slice, a map, an iterator, or a count that indexes
// the list), one pass of whose body reads a kept decision — a field of it, a method of it, the
// value whole as an argument or a switch's tag, a conversion or a copy — must ask the predicate,
// in that pass, of each decision the pass reads (readKeptLoop, askOfEach). The rule names a
// decision by the path it is read through (keptPath): a variable (a range's value or key, a
// parameter, a local), then each field, each element at an index and each method of no arguments
// taken of it, down to the first whose type is one decision, an index named by its variable or
// its constant. So d.Ref reads d, marks[i].Actor reads marks[i] at the variable i, and
// owed.Decisions[i] reads that element of that list. The pass asks of a decision when it calls
// the predicate with a workflow.Decision whose expression names that path, or names a value that
// holds the decision in a field (o, of o.mark), or when it calls a function of the walk with it at
// a position that function hands on to such a call, however many calls down (resolve):
// keptSuperseded hands its mark to RefSuperseded inside a workflow.Decision, and RefSuperseded
// hands its d to the predicate. So a pass that reads its own decision and asks of another — a
// fixed one (marks[0], a parameter), another list's element at the same index, or the kept
// approval — asks nothing of its own. A call before or after the loop asks nothing of its
// decisions, and an ask in a loop nested in it answers for that nested loop alone, whose body is a
// pass of its own (ownPass). A loop that renders nothing a person or architect reads passes only
// where keptLoopReaders names it (unaskedKeptLoops).
//
// And no function writes a list of kept decisions whole into text (wholeListRenders): a value
// whose type holds a kept decision and is not one (carriesKept: a pointer, an array, a slice or a
// channel of what holds one, a map whose key or value holds one, or a struct with a field that
// holds one, through any cycle of named types, such as record.KeptDecisions or a type of the
// function's own that holds it), given as an argument fmt prints (its Sprint, Print, Fprint and
// Append families, and Errorf), or at a position of a function of the walk that hands that
// parameter whole to such a print, however many calls down (resolve). No ask can be of each
// decision of a value printed as one, so this holds in a loop that asks as much as outside any
// loop, and it holds a range over a map of lists that prints each list. A value read for a field
// that holds no decision (owed.Approved, owed.Step) is no such value, nor is a parameter a
// function reads only such fields of.
//
// The rule reads the source as written, and holds what it follows and no more; it is no proof of
// where a value flows or of what runs:
//
//   - An ask counts for every decision its argument names, whatever the call does with them: an
//     argument built from the decision and a fixed one (pick(d, first)), or from one field of the
//     decision, counts as an ask of the decision. Nor does the rule read what the pass does with
//     the answer: a loop that asks of its decision and renders it either way passes.
//   - A decision copied into another variable in the pass, or a workflow.Decision built into a
//     variable before the predicate is called with it, names nothing of the decision it was built
//     from, so the pass must ask of a decision under each name it reads it by (errs toward
//     failing).
//   - A decision the pass reaches through a call with arguments (lookup(i)), or at an index that
//     is no variable, field or constant (marks[len(marks)-1]), has no path to tie an ask to, so a
//     loop that reads one fails (errs toward failing).
//   - Only a call of a function by its name is followed, into a function of the three packages
//     or of the planted package: an ask through a function value, a method of an interface
//     (record.Store's) or a function of another package is not, so a loop that asks only through
//     one fails (errs toward failing); and a list handed whole to such a function is not read as
//     written into text (errs toward passing), such as a list given to a logger as an attribute
//     (slog), encoded by json.Marshal, or printed by another package's Errorf (wait.Errorf).
//   - A value is read by its static type. A list kept in a variable or a field of an interface
//     type (any, error, record.OutboxPayload) or of a type parameter is not read as a list, nor
//     is one reached through reflection, except where it is converted at the print
//     (fmt.Sprint(any(marks))) or handed to a parameter that a function of the walk prints whole
//     (a helper that prints its any parameter). What fmt reads of a value at run time is not read
//     either way: a list printed by %T or %p, or of a type whose String method prints no decision,
//     still fails (errs toward failing), and a String method that prints decisions its value does
//     not hold is not read (errs toward passing).
//   - A function literal is read where it is written: as part of the pass it is written in,
//     wherever it runs, and as no part of a pass that only calls it.
//   - A single decision read outside any loop is held to nothing here: the senders the test reads
//     are held to the predicate as functions, through what they call.

// keptDecisionTypes are the record types that hold one kept decision: the row the daemon keeps
// (record.ParkedDecision) and the mark a task names it by (record.DecisionMark). A loop that
// reads a value of either type reads a kept decision, whatever the loop runs over (readKeptLoop).
var keptDecisionTypes = []string{"ParkedDecision", "DecisionMark"}

// keptLoopName names one loop of keptLoopReaders: its function, as go/types names it
// (types.Func.FullName), and its clause as gofmt prints it (loopClause).
type keptLoopName struct{ fn, clause string }

// keptLoopReaders is every loop of the three packages one pass of whose body reads a kept
// decision (record.ParkedDecision or record.DecisionMark) and does not ask the predicate of each
// decision it reads in that pass, each named by its function and its clause (keptLoopName), with
// why the loop renders nothing an architect or person reads. A row allows that one loop: another
// such loop of the same function needs a row of its own, a clause two such loops share allows
// neither, and a row whose loop came to ask, changed its clause or went is taken out
// (unaskedKeptLoops).
var keptLoopReaders = map[keptLoopName]string{
	{senderModule + "workflow.ParkedDecisionTask", "for _, d := range decisions"}: "it turns each unclaimed row into its mark " +
		"(record.ParkedDecision.Mark), the list ForgetSuperseded and KeptDecisionsTask each ask of in a loop of their own, and renders nothing",
	{senderModule + "workflow.ReadKeptGates", "for _, d := range owed.Decisions"}: "it collects the issue each decision was made on, " +
		"to read their gates in one read (record.Store.Gates), and renders nothing",
	{"(*" + senderModule + "workflow.Engine).warnKeptDecisionsOfAClosedTree", "for _, decision := range kept"}: "it writes each " +
		"decision a closing tree still owes into the daemon's log (keptDecisionOfAClosedTree), which no architect or person is " +
		"sent; the controller was told each when it was kept",
	{"(*" + senderModule + "daemon.workflowRuntime).rootTask", "for _, mark := range named.Decisions"}: "it drops the marks a " +
		"held task named that its delivery no longer holds and sorts the rest into those whose issue stands in the tree (stays) " +
		"and those whose issue left it (left), rendering nothing: what stays is asked and written again by " +
		"workflow.KeptDecisionsTask, and what left is released to the tree it is owed to now, whose catch-up asks it " +
		"(workflow.CatchUpTheTreesOwed)",
}

// keptLoop is a loop of a function one pass of whose body reads a kept decision: where it is,
// its clause (loopClause), each decision the pass reads (keptReads) and each call the pass makes
// of a function it names (callSites); and, once the walk has read every function (resolve),
// whether the pass asks the predicate of each decision it reads, and the reads it asks nothing of.
type keptLoop struct {
	at, clause string
	reads      []keptRead
	sites      []callSite
	asks       bool
	unasked    []string
}

// keptRead is one read of a kept decision in a pass: the expression as written, and the path
// that names the decision it reads (keptPath), "" where no path names it.
type keptRead struct{ expr, root string }

// wholeRender is a list of kept decisions a function writes whole into text: the expression as
// written, the function it hands it to, as go/types names it, and where that call is.
type wholeRender struct{ expr, to, at string }

// callSite is one call a body makes of a function it names (calledFunc): the function, its
// FullName (its generic origin's), where the call is, and what each of the function's positions
// receives. Position 0 is the receiver of a method, and each parameter follows in order; a
// variadic parameter's position receives every argument from it on.
type callSite struct {
	fun        *types.Func
	callee, at string
	args       []positioned
}

// positioned is an argument of a call and the position (callSite) it is given at.
type positioned struct {
	pos  int
	expr ast.Expr
}

// unaskedKeptLoops holds w's loops that read a kept decision to the rule for them (this file's
// doc), given allowed, the loops that may ask nothing (keptLoopReaders): notes says why each
// allowed loop asks nothing, and failures names each loop whose own pass does not ask the
// predicate of each decision it reads and that no row allows, then each row of allowed that names
// no such loop or more than one.
func (w *decisionWalk) unaskedKeptLoops(allowed map[keptLoopName]string) (notes, failures []string) {
	found := map[keptLoopName][]string{}
	for _, f := range w.funcs {
		for _, loop := range f.keptLoops {
			if loop.asks {
				continue
			}
			name := keptLoopName{f.name, loop.clause}
			why, named := allowed[name]
			if !named {
				failures = append(failures, fmt.Sprintf("%s (%s) reads kept decisions in the loop `%s` at %s, but its own pass does not "+
					"ask %s of %s, directly or through a function it hands the decision to: it can name a decision without knowing "+
					"whether it is still the person's word. Ask the predicate in the loop's own body of each decision the body reads, "+
					"or name the loop in keptLoopReaders with why it renders nothing", f.short(), f.at, loop.clause, loop.at,
					short(decisionPredicate), strings.Join(loop.unasked, ", ")))
				continue
			}
			found[name] = append(found[name], loop.at)
			notes = append(notes, fmt.Sprintf("kept-decision loop that asks nothing, `%s` of %s at %s: %s", loop.clause, f.short(), loop.at, why))
		}
	}
	var rows []string
	for name := range allowed {
		switch at := found[name]; {
		case len(at) == 0:
			rows = append(rows, fmt.Sprintf("keptLoopReaders names the loop `%s` of %s, which holds no loop of that clause that reads "+
				"a kept decision and asks nothing of it: take the row out, so the list names every such loop and no other",
				name.clause, short(name.fn)))
		case len(at) > 1:
			rows = append(rows, fmt.Sprintf("keptLoopReaders names the loop `%s` of %s, which holds %d loops of that clause that read "+
				"a kept decision and ask nothing of it (at %s): a row allows one loop, so ask the predicate in the others or give each "+
				"a clause of its own", name.clause, short(name.fn), len(at), strings.Join(at, ", ")))
		}
	}
	slices.Sort(rows)
	return notes, append(failures, rows...)
}

// wholeListRenders names each list of kept decisions a function of w writes whole into text
// (resolve), in a loop or outside one: no ask can be of each decision of a value printed as one.
func (w *decisionWalk) wholeListRenders() []string {
	var failures []string
	for _, f := range w.funcs {
		for _, render := range f.renders {
			failures = append(failures, fmt.Sprintf("%s (%s) hands `%s`, which holds kept decisions, whole to %s at %s, which writes it "+
				"into text: no ask of %s can be of each decision of a value written as one, so the text can name a decision that is "+
				"no longer the person's word. Write each decision in a loop that asks the predicate of it", f.short(), f.at,
				render.expr, short(render.to), render.at, short(decisionPredicate)))
		}
	}
	return failures
}

// asksOfParameter is whether the function short names (short) asks the predicate of its
// parameter named param, itself or through the functions it calls (resolve).
func (w *decisionWalk) asksOfParameter(name, param string) bool {
	f := w.named(name)
	if f == nil {
		return false
	}
	for pos, object := range f.params {
		if object != nil && object.Name() == param && w.asking[f.name][pos] {
			return true
		}
	}
	return false
}

// resolve reads, once every function of w is read, what each function asks the predicate of and
// writes whole into text through the functions it calls by name, to a fixed point. A position of
// a function asks (asking) where the function names that parameter in an argument it gives at a
// position that asks, the predicate's workflow.Decision first (asksAt); and a position writes into
// text (rendering) where the function gives that parameter whole at a position that writes into
// text (paramRendered), fmt's printed arguments first (rendersAt). Then it holds each function's
// calls to the lists they write whole into text (renders) and each of its loops to what the loop's
// pass asks (askOfEach).
func (w *decisionWalk) resolve() {
	w.asking, w.rendering = map[string]map[int]bool{}, map[string]map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range w.funcs {
			for _, site := range f.sites {
				for _, arg := range site.args {
					if w.asksAt(site, arg.pos) {
						for pos, param := range f.params {
							if param != nil && !w.asking[f.name][pos] && mentions(f.info, arg.expr, param) {
								setPosition(w.asking, f.name, pos)
								changed = true
							}
						}
					}
					if w.rendersAt(site, arg.pos) {
						if pos := paramRendered(f.info, arg.expr, f.params); pos >= 0 && !w.rendering[f.name][pos] {
							setPosition(w.rendering, f.name, pos)
							changed = true
						}
					}
				}
			}
		}
	}
	for _, f := range w.funcs {
		for _, site := range f.sites {
			for _, arg := range site.args {
				if w.rendersAt(site, arg.pos) && keptList(f.info.TypeOf(asWritten(f.info, arg.expr))) {
					f.renders = append(f.renders, wholeRender{expr: types.ExprString(arg.expr), to: site.callee, at: site.at})
				}
			}
		}
		for i := range f.keptLoops {
			w.askOfEach(f.info, &f.keptLoops[i])
		}
	}
}

// askOfEach holds loop, once w knows what each function asks (resolve), to the rule for a pass:
// the pass asks of a decision it reads when a call it makes names that decision's path
// (keptPath), or a value that holds it in a field (asksOf), in an argument at a position that asks
// (asksAt). A read no path names is asked of by nothing.
func (w *decisionWalk) askOfEach(info *types.Info, loop *keptLoop) {
	var asked []string
	for _, site := range loop.sites {
		for _, arg := range site.args {
			if !w.asksAt(site, arg.pos) {
				continue
			}
			ast.Inspect(arg.expr, func(n ast.Node) bool {
				if expr, ok := n.(ast.Expr); ok {
					if path, _ := keptPath(info, expr); path != "" {
						asked = append(asked, path)
					}
				}
				return true
			})
		}
	}
	loop.unasked = nil
	for _, read := range loop.reads {
		if read.root != "" && slices.ContainsFunc(asked, func(path string) bool { return asksOf(path, read.root) }) {
			continue
		}
		unasked := "`" + read.expr + "`"
		if read.root == "" {
			unasked += ", which no variable names"
		}
		if !slices.Contains(loop.unasked, unasked) {
			loop.unasked = append(loop.unasked, unasked)
		}
	}
	loop.asks = len(loop.reads) > 0 && len(loop.unasked) == 0
}

// asksOf is whether an ask that names the path asked asks of the decision root names: root is
// that path, or a decision held in a field of what asked names, reached from it by fields and
// methods alone (o.mark, of an o asked). An element of a list asked whole (marks[i], of marks) is
// not, since the ask can be of any element.
func asksOf(asked, root string) bool {
	rest, inside := strings.CutPrefix(root, asked+".")
	return root == asked || inside && !strings.Contains(rest, "[")
}

// asksAt is whether site's function asks the predicate of what it is given at pos: the
// predicate's workflow.Decision (predicateAsks), or a position resolve found asking.
func (w *decisionWalk) asksAt(site callSite, pos int) bool {
	if site.callee == decisionPredicate {
		return predicateAsks(site.fun, pos)
	}
	return w.asking[site.callee][pos]
}

// rendersAt is whether site's function writes what it is given at pos whole into text: an
// argument fmt prints (printsText), or a position resolve found printed.
func (w *decisionWalk) rendersAt(site callSite, pos int) bool {
	if printsText(site.callee) {
		sig := site.fun.Type().(*types.Signature)
		return sig.Variadic() && pos == sig.Params().Len()
	}
	return w.rendering[site.callee][pos]
}

// predicateAsks is whether pos, of a call of the predicate (callSite), is its workflow.Decision:
// the decision it is asked of. Its gate, and whether a person approves the step, are what it asks
// of that decision.
func predicateAsks(predicate *types.Func, pos int) bool {
	params := predicate.Type().(*types.Signature).Params()
	if pos < 1 || pos > params.Len() {
		return false
	}
	named, ok := types.Unalias(params.At(pos - 1).Type()).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == senderModule+"workflow" && named.Obj().Name() == "Decision"
}

// printsText is whether the function name names (types.Func.FullName) is one of fmt's that write
// their variadic arguments into text: the Sprint, Print, Fprint and Append families, and Errorf.
func printsText(name string) bool {
	rest, inFmt := strings.CutPrefix(name, "fmt.")
	return inFmt && (rest == "Errorf" || slices.ContainsFunc([]string{"Sprint", "Print", "Fprint", "Append"},
		func(family string) bool { return strings.HasPrefix(rest, family) }))
}

// paramRendered is the position (callSite) of the parameter of params that arg gives whole to
// what prints it: the parameter itself, whatever its type, since a parameter of type any, of an
// interface or of a type parameter prints whatever its caller gives it; or a field, an element or
// a slice of it that holds kept decisions and is not one (keptList). It is -1 for anything else.
// Parentheses, a dereference, an address taken and a conversion pass the value through
// (asWritten).
func paramRendered(info *types.Info, arg ast.Expr, params []types.Object) int {
	arg = asWritten(info, arg)
	base, projected := arg, false
	for {
		switch x := base.(type) {
		case *ast.SelectorExpr:
			if selection := info.Selections[x]; selection != nil && selection.Kind() == types.FieldVal {
				base, projected = asWritten(info, x.X), true
				continue
			}
		case *ast.IndexExpr:
			base, projected = asWritten(info, x.X), true
			continue
		case *ast.SliceExpr:
			base, projected = asWritten(info, x.X), true
			continue
		}
		break
	}
	ident, ok := base.(*ast.Ident)
	if !ok || info.Uses[ident] == nil || projected && !keptList(info.TypeOf(arg)) {
		return -1
	}
	return slices.Index(params, info.Uses[ident])
}

// asWritten is expr without what passes its value through to a print unchanged: parentheses, a
// dereference, an address taken and a conversion.
func asWritten(info *types.Info, expr ast.Expr) ast.Expr {
	for {
		switch x := expr.(type) {
		case *ast.ParenExpr:
			expr = x.X
			continue
		case *ast.StarExpr:
			expr = x.X
			continue
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				expr = x.X
				continue
			}
		case *ast.CallExpr:
			if len(x.Args) == 1 && info.Types[x.Fun].IsType() {
				expr = x.Args[0]
				continue
			}
		}
		return expr
	}
}

// mentions is whether expr names object.
func mentions(info *types.Info, expr ast.Expr, object types.Object) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && info.Uses[ident] == object {
			found = true
		}
		return !found
	})
	return found
}

// setPosition adds pos to what positions holds for the function name.
func setPosition(positions map[string]map[int]bool, name string, pos int) {
	if positions[name] == nil {
		positions[name] = map[int]bool{}
	}
	positions[name][pos] = true
}

// readKeptLoop reads loop, a for or a range statement, when one pass of its body reads a kept
// decision (keptReads), whatever the loop runs over (a slice, a map, an iterator) or however it
// counts (a C-style for that indexes the list): each decision the pass reads and each call the
// pass makes of a function it names, which askOfEach holds to each other. A pass is the body as
// ownPass walks it: a call before or after the loop asks nothing of its decisions, and an ask in a
// loop nested in it answers for that nested loop alone. written is what the function's
// assignments write (writtenExprs).
func readKeptLoop(pkg *checkedPackage, loop ast.Node, written map[ast.Expr]bool) (keptLoop, bool) {
	var body *ast.BlockStmt
	switch loop := loop.(type) {
	case *ast.RangeStmt:
		body = loop.Body
	case *ast.ForStmt:
		body = loop.Body
	}
	pass := func(visit func(ast.Node) bool) { ownPass(body, visit) }
	reads := keptReads(pkg.info, written, pass)
	if len(reads) == 0 {
		return keptLoop{}, false
	}
	return keptLoop{at: pkg.pos(loop), clause: loopClause(pkg.fset, loop), reads: reads, sites: callSites(pkg, pass)}, true
}

// keptReads is each read of a kept decision in pass, one pass of a loop (ownPass): every
// expression whose value is one (readsKeptDecision), each with the path that names it (keptPath).
// A read no path names is one with no root, unless a decision inside it has one (a literal or a
// call built from d reads d), which is read in its own right.
func keptReads(info *types.Info, written map[ast.Expr]bool, pass func(visit func(ast.Node) bool)) []keptRead {
	var reads []keptRead
	pass(func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok || !readsKeptDecision(info, written, expr) {
			return true
		}
		_, root := keptPath(info, expr)
		if root == "" && holdsNamedRead(info, written, expr) {
			return true
		}
		reads = append(reads, keptRead{expr: types.ExprString(expr), root: root})
		return true
	})
	return reads
}

// readsKeptDecision is whether expr reads a kept decision: a value of one of keptDecisionTypes
// (isKeptDecision) that is not what an assignment writes (written) — a field of it, a method of
// it, the value whole as an argument or a switch's tag, a conversion or a copy.
func readsKeptDecision(info *types.Info, written map[ast.Expr]bool, expr ast.Expr) bool {
	tv, ok := info.Types[expr]
	return ok && !written[expr] && tv.IsValue() && isKeptDecision(tv.Type)
}

// holdsNamedRead is whether expr holds, other than itself, a read of a kept decision that a path
// names (keptPath).
func holdsNamedRead(info *types.Info, written map[ast.Expr]bool, expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if inner, ok := n.(ast.Expr); ok && inner != expr && readsKeptDecision(info, written, inner) {
			_, root := keptPath(info, inner)
			found = root != ""
		}
		return !found
	})
	return found
}

// keptPath is the path expr reads: a variable, of the function or of a package, then each field,
// each element at an index and each method of no arguments taken of it, each named by its object
// (objectKey), and an index by its constant's value or its own path (indexKey); parentheses, a
// dereference and a conversion take nothing. root is the shortest prefix of that path whose type
// is one kept decision, which names the decision expr reads: d of d.Ref, marks[i] of
// marks[i].Actor, row.Mark of row.Mark.Ref. Both are "" where expr is no such path (a call with
// arguments, a literal, a computed index), and root is "" where no prefix is a kept decision.
func keptPath(info *types.Info, expr ast.Expr) (path, root string) {
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return keptPath(info, x.X)
	case *ast.StarExpr:
		path, root = keptPath(info, x.X)
	case *ast.Ident:
		path = variableKey(info.Uses[x])
	case *ast.SelectorExpr:
		switch selection := info.Selections[x]; {
		case selection == nil:
			path = variableKey(info.Uses[x.Sel])
		case selection.Kind() == types.FieldVal:
			if path, root = keptPath(info, x.X); path != "" {
				path += "." + objectKey(selection.Obj())
			}
		}
	case *ast.IndexExpr:
		if index := indexKey(info, x.Index); index != "" {
			if path, root = keptPath(info, x.X); path != "" {
				path += "[" + index + "]"
			}
		}
	case *ast.CallExpr:
		if len(x.Args) == 1 && info.Types[x.Fun].IsType() {
			path, root = keptPath(info, x.Args[0])
		} else if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok && len(x.Args) == 0 {
			if selection := info.Selections[sel]; selection != nil && selection.Kind() == types.MethodVal {
				if path, root = keptPath(info, sel.X); path != "" {
					path += "." + objectKey(selection.Obj()) + "()"
				}
			}
		}
	}
	if path == "" {
		return "", ""
	}
	if root == "" && isKeptDecision(info.TypeOf(expr)) {
		root = path
	}
	return path, root
}

// indexKey names an index in a path (keptPath): its constant's value, or its own path (a
// variable, or a field of one); "" for any other index, which names no element.
func indexKey(info *types.Info, index ast.Expr) string {
	if value := info.Types[index].Value; value != nil {
		return value.ExactString()
	}
	path, _ := keptPath(info, index)
	return path
}

// variableKey names a variable that is no field (objectKey), "" for any other object.
func variableKey(object types.Object) string {
	if v, ok := object.(*types.Var); ok && !v.IsField() {
		return objectKey(v)
	}
	return ""
}

// objectKey names object in a path: by its name, for whoever reads a failure, and by its
// identity, so two variables or fields of one name are two.
func objectKey(object types.Object) string { return fmt.Sprintf("%s@%p", object.Name(), object) }

// ownPass walks one pass of the loop whose body is body, as ast.Inspect walks a node: every
// statement of the body, and of a loop nested in it only what runs in that pass, its range
// expression or its init, condition and post. The nested loop's body is a pass of that loop's
// own, which readDecisionFunc reads as a loop of its own, and the key and value a nested range
// assigns are written, not read.
func ownPass(body *ast.BlockStmt, visit func(ast.Node) bool) {
	var inspect func(ast.Node) bool
	inspect = func(n ast.Node) bool {
		var clause []ast.Node
		switch loop := n.(type) {
		case *ast.RangeStmt:
			clause = []ast.Node{loop.X}
		case *ast.ForStmt:
			clause = []ast.Node{loop.Init, loop.Cond, loop.Post}
		default:
			return visit(n)
		}
		for _, part := range clause {
			if part != nil {
				ast.Inspect(part, inspect)
			}
		}
		return false
	}
	ast.Inspect(body, inspect)
}

// loopClause is loop's clause as gofmt prints it, without its body: `for _, d := range
// owed.Decisions`. keptLoopReaders names a loop by it, so a row stays on its loop as lines move,
// and names no loop once the clause changes.
func loopClause(fset *token.FileSet, loop ast.Node) string {
	var bare ast.Node
	switch loop := loop.(type) {
	case *ast.RangeStmt:
		cut := *loop
		cut.Body = &ast.BlockStmt{}
		bare = &cut
	case *ast.ForStmt:
		cut := *loop
		cut.Body = &ast.BlockStmt{}
		bare = &cut
	}
	var text strings.Builder
	if err := printer.Fprint(&text, fset, bare); err != nil {
		return fmt.Sprintf("the loop at %s, whose clause did not print: %v", fset.Position(loop.Pos()), err)
	}
	clause := text.String()
	if brace := strings.LastIndex(clause, "{"); brace >= 0 {
		clause = clause[:brace]
	}
	return strings.TrimSpace(clause)
}

// callSites is every call of a function by name (calledFunc) that walk visits, with what each
// position of the function receives (callSite). walk is ast.Inspect of a function's body, or one
// pass of a loop (ownPass). A call of a function value, a builtin or a conversion is none.
func callSites(pkg *checkedPackage, walk func(visit func(ast.Node) bool)) []callSite {
	var sites []callSite
	walk(func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := calledFunc(pkg.info, call)
		if fun == nil {
			return true
		}
		site := callSite{fun: fun, callee: fun.Origin().FullName(), at: pkg.pos(call)}
		args := call.Args
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			if selection := pkg.info.Selections[sel]; selection != nil {
				switch selection.Kind() {
				case types.MethodVal:
					site.args = append(site.args, positioned{0, sel.X})
				case types.MethodExpr:
					if len(args) > 0 {
						site.args = append(site.args, positioned{0, args[0]})
						args = args[1:]
					}
				}
			}
		}
		sig := fun.Type().(*types.Signature)
		for i, arg := range args {
			param := i
			if sig.Variadic() && param >= sig.Params().Len()-1 {
				param = sig.Params().Len() - 1
			}
			site.args = append(site.args, positioned{1 + param, arg})
		}
		sites = append(sites, site)
		return true
	})
	return sites
}

// funcParams is fn's receiver and parameters by their position in a call (callSite): position 0
// its receiver, then each parameter; nil where fn has no receiver or a position has no name.
func funcParams(info *types.Info, fn *ast.FuncDecl) []types.Object {
	params := []types.Object{nil}
	if fn.Recv != nil {
		for _, field := range fn.Recv.List {
			for _, name := range field.Names {
				params[0] = info.Defs[name]
			}
		}
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			params = append(params, nil)
		}
		for _, name := range field.Names {
			params = append(params, info.Defs[name])
		}
	}
	return params
}

// isKeptDecision is whether typ is one of the record types that hold one kept decision, or a
// pointer to one.
func isKeptDecision(typ types.Type) bool {
	return slices.ContainsFunc(keptDecisionTypes, func(name string) bool { return isRecordType(typ, name) })
}

// keptList is whether a value of typ holds kept decisions and is not one (carriesKept): a list,
// a map, a pointer to one, or a struct that holds one, which no ask can be of each decision of.
func keptList(typ types.Type) bool {
	return typ != nil && !isKeptDecision(typ) && carriesKept(typ, map[*types.Named]bool{})
}

// carriesKept is whether a value of typ holds a kept decision: is one (isKeptDecision), or is a
// pointer, an array, a slice or a channel of what holds one, a map whose key or value holds one,
// or a struct with a field that holds one, through any number of named types. seen holds the
// named types already read, so a cycle of them (a thread whose replies are threads) is read once.
// An interface or a type parameter holds nothing by its type, whatever it holds at run time.
func carriesKept(typ types.Type, seen map[*types.Named]bool) bool {
	typ = types.Unalias(typ)
	if isKeptDecision(typ) {
		return true
	}
	if named, ok := typ.(*types.Named); ok {
		if seen[named] {
			return false
		}
		seen[named] = true
	}
	switch t := typ.Underlying().(type) {
	case *types.Pointer:
		return carriesKept(t.Elem(), seen)
	case *types.Slice:
		return carriesKept(t.Elem(), seen)
	case *types.Array:
		return carriesKept(t.Elem(), seen)
	case *types.Chan:
		return carriesKept(t.Elem(), seen)
	case *types.Map:
		return carriesKept(t.Key(), seen) || carriesKept(t.Elem(), seen)
	case *types.Struct:
		for i := range t.NumFields() {
			if carriesKept(t.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// A loop that reads a kept decision and does not ask of each passes only where a row of
// keptLoopReaders names it, by its function and its clause, and a row allows that one loop
// (unaskedKeptLoops). Each row here plants a function render beside the planted rows' helpers
// (plantedDecisionPreamble), allows loops of it in a list of its own, and names a phrase of each
// failure the rule must write, in order.
func TestDecisionSendersAllowAnUnaskedKeptLoopOnlyByItsOwnRow(t *testing.T) {
	t.Parallel()
	planted := newPlantedDecisionSource(t)
	const collects = `
func render(marks []record.DecisionMark) []string {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	return issues
}
`
	const twoLoops = `
func render(marks []record.DecisionMark) (string, []string) {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	var named []string
	for _, other := range marks {
		named = append(named, fmt.Sprintf("%s by %s", other.Ref, other.Actor))
	}
	return joined(named), issues
}
`
	const sharedClause = `
func render(marks []record.DecisionMark) (string, []string) {
	var issues []string
	for _, d := range marks {
		issues = append(issues, d.Issue)
	}
	var named []string
	for _, d := range marks {
		named = append(named, fmt.Sprintf("%s by %s", d.Ref, d.Actor))
	}
	return joined(named), issues
}
`
	const asking = `
func render(gate *record.DesignGate, marks []record.DecisionMark) string {
	var named []string
	for _, d := range marks {
		if stands(gate, d) {
			named = append(named, line(d))
		}
	}
	return joined(named)
}
`
	collectsLoop := keptLoopName{plantedDecisionRender, "for _, d := range marks"}
	for _, row := range []struct {
		name    string
		source  string
		allowed []keptLoopName
		fails   []string
	}{
		{name: "a row allows its one loop", source: collects, allowed: []keptLoopName{collectsLoop}},
		{name: "a second loop of the function that asks nothing fails", source: twoLoops, allowed: []keptLoopName{collectsLoop},
			fails: []string{"in the loop `for _, other := range marks`"}},
		{name: "a row whose clause two such loops share fails", source: sharedClause, allowed: []keptLoopName{collectsLoop},
			fails: []string{"holds 2 loops of that clause"}},
		{name: "a row whose loop asks is taken out", source: asking, allowed: []keptLoopName{collectsLoop},
			fails: []string{"holds no loop of that clause"}},
		{name: "a row whose clause its loop no longer has is taken out", source: collects,
			allowed: []keptLoopName{{plantedDecisionRender, "for _, mark := range marks"}},
			fails:   []string{"in the loop `for _, d := range marks`", "holds no loop of that clause"}},
		{name: "a row of a function the walk does not hold is taken out", source: collects,
			allowed: []keptLoopName{collectsLoop, {plantedDecisionPackage + ".gone", "for _, d := range marks"}},
			fails:   []string{"holds no loop of that clause"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			allowed := map[keptLoopName]string{}
			for _, name := range row.allowed {
				allowed[name] = "planted"
			}
			_, failures := planted.walk(t, row.source).unaskedKeptLoops(allowed)
			if len(failures) != len(row.fails) {
				t.Fatalf("the rule wrote %d failures, want %d (%q): %q", len(failures), len(row.fails), row.fails, failures)
			}
			for i, phrase := range row.fails {
				if !strings.Contains(failures[i], phrase) {
					t.Errorf("failure %d is %q, want it to say %q", i, failures[i], phrase)
				}
			}
		})
	}
}
