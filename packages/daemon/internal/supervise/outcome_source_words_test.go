package supervise

import (
	"go/ast"
	"go/constant"
	gotoken "go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// assignments is every expression the package's source gives each variable or constant: by := and
// =, by a var or const declaration, by a range — its value, and its key where the key is an element
// too — and, for a parameter, by each call of its function. A value no expression stands for alone
// — one of several results of one call, an element of what a loop ranges over — is recorded as that
// call or as what it ranges over. It is read once per package (given).
func (s *goSource) assignments() map[types.Object][]ast.Expr {
	if s.given != nil {
		return s.given
	}
	given := map[types.Object][]ast.Expr{}
	give := func(target, expr ast.Expr) {
		if ident, ok := ast.Unparen(target).(*ast.Ident); ok {
			if object := s.info.ObjectOf(ident); object != nil {
				given[object] = append(given[object], expr)
			}
		}
	}
	decls := s.funcs()
	for _, file := range s.files {
		ast.Inspect(file, func(node ast.Node) bool {
			paired(node, give)
			switch node := node.(type) {
			case *ast.RangeStmt:
				// The key is an index of a string, a slice, an array or an integer's count, and no
				// part of what the loop ranges over; of a map, a channel or an iterator it is an
				// element, as the value is.
				if node.Value != nil {
					give(node.Value, node.X)
				}
				if typ := s.info.Types[node.X].Type; node.Key != nil && typ != nil {
					switch typ.Underlying().(type) {
					case *types.Map, *types.Chan, *types.Signature:
						give(node.Key, node.X)
					}
				}
			case *ast.CallExpr:
				fn := s.callee(node)
				decl, declared := decls[fn]
				if !declared {
					return true
				}
				for i, param := range s.params(decl) {
					if param != nil && i < len(node.Args) {
						given[param] = append(given[param], node.Args[i])
					}
				}
			}
			return true
		})
	}
	s.given = given
	return given
}

// fieldOf is one field of one variable, which the test follows a refusal's words into within the
// function that declares the variable (fieldAssignments).
type fieldOf struct{ variable, field types.Object }

// fieldAssignments is every expression a function gives a field of one of its own variables: a
// struct literal given to the variable by :=, = or a var declaration, or the literal's address,
// gives each field it names, or each field in order where it names none; and an assignment to
// x.F gives F of x. A variable of the package outside any function belongs to no one function,
// and a value handed to another function's parameter is that function's, so the test follows the
// words into a field of neither (TestOutcomeSourceHoldsThePolicyAtItsSites). A value no
// expression stands for alone is recorded as assignments records it (paired).
func (s *goSource) fieldAssignments() map[fieldOf][]ast.Expr {
	given := map[fieldOf][]ast.Expr{}
	local := func(ident *ast.Ident) types.Object {
		variable, ok := s.info.ObjectOf(ident).(*types.Var)
		if !ok || variable.Parent() == nil || variable.Parent() == s.pkg.Scope() {
			return nil
		}
		return variable
	}
	add := func(variable, field types.Object, expr ast.Expr) {
		given[fieldOf{variable, field}] = append(given[fieldOf{variable, field}], expr)
	}
	literal := func(variable types.Object, expr ast.Expr) {
		expr = ast.Unparen(expr)
		if address, ok := expr.(*ast.UnaryExpr); ok && address.Op == gotoken.AND {
			expr = ast.Unparen(address.X)
		}
		lit, ok := expr.(*ast.CompositeLit)
		if !ok || s.info.Types[lit].Type == nil {
			return
		}
		fields, ok := s.info.Types[lit].Type.Underlying().(*types.Struct)
		if !ok {
			return
		}
		for i, elt := range lit.Elts {
			if pair, keyed := elt.(*ast.KeyValueExpr); keyed {
				if key, ok := pair.Key.(*ast.Ident); ok && s.info.Uses[key] != nil {
					add(variable, s.info.Uses[key], pair.Value)
				}
			} else if i < fields.NumFields() {
				add(variable, fields.Field(i), elt)
			}
		}
	}
	give := func(target, expr ast.Expr) {
		switch target := ast.Unparen(target).(type) {
		case *ast.Ident:
			if variable := local(target); variable != nil {
				literal(variable, expr)
			}
		case *ast.SelectorExpr:
			x, ok := ast.Unparen(target.X).(*ast.Ident)
			if selection := s.info.Selections[target]; ok && selection != nil && selection.Kind() == types.FieldVal {
				if variable := local(x); variable != nil {
					add(variable, selection.Obj(), expr)
				}
			}
		}
	}
	for _, file := range s.files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				paired(node, give)
				return true
			})
		}
	}
	return given
}

// paired hands give each target node gives a value, with the expression that gives it: an
// assignment or a var or const declaration gives each target the expression in its place where
// it has as many expressions as targets, and every target its one expression where one gives
// them all, such as a call of several results. Any other node gives nothing.
func paired(node ast.Node, give func(target, expr ast.Expr)) {
	var lhs, rhs []ast.Expr
	switch node := node.(type) {
	case *ast.AssignStmt:
		lhs, rhs = node.Lhs, node.Rhs
	case *ast.ValueSpec:
		lhs = make([]ast.Expr, len(node.Names))
		for i, name := range node.Names {
			lhs[i] = name
		}
		rhs = node.Values
	}
	for i, left := range lhs {
		if len(lhs) == len(rhs) {
			give(left, rhs[i])
		} else if len(rhs) == 1 {
			give(left, rhs[0])
		}
	}
}

// refusalTaint is every variable of the package a refusal's words reach, through assignments,
// through the loops that range over a list or a map of them and through the parameters of the
// package's own functions, since a helper can take the words as its parameter. It holds only a
// variable that holds the words as its value (carriesText), and, of a variable whose words a field
// holds, that field alone, within the function that declares the variable (fieldAssignments).
type refusalTaint struct {
	src *goSource
	// fields is each field that holds a refusal's words, and methods the names of the methods and
	// functions that return them.
	fields  func(field *types.Var) bool
	methods []string
	// answered is each variable of type error that holds the error a prompt send answers, whose
	// Error method returns the agent's words (sendAnswer).
	answered map[types.Object]bool
	tainted  map[types.Object]bool
	// taintedFields is each field of a variable the words reach.
	taintedFields map[fieldOf]bool
}

// refusalWords is the taint of the words a refusal carries in the package. In each of the four
// packages they are the error a prompt send answers (sendAnswer), whose Error method returns them.
// In this package they are also the Error of a late refusal and of a prompt result, the Err of a
// refused send, the send error a delivery records, the Message of a turn's outcome and of the
// report that carried it (RequestTurnOutcome), which hold the provider's words a claim's
// LastRefusal returns, and whatever LastRefusal, runtime.RefusalReason or a RefusalReason method
// returns. In the runtime, which reads the words from a send's error, and in the stream and the
// bridge, which handle the frames themselves, they are also every string field named Error,
// Reason or Message — the words of Oh My Pi's response and of its prompt_result's error, of a
// shim's own answer, and of the stream's events — and whatever a Reason or RefusalReason method or
// runtime.RefusalReason returns.
func (s *goSource) refusalWords(t *testing.T) *refusalTaint {
	t.Helper()
	taint := &refusalTaint{src: s, answered: map[types.Object]bool{}, tainted: map[types.Object]bool{}, taintedFields: map[fieldOf]bool{}}
	if s.pkg.Path() == supervisePath {
		fields := map[types.Object]bool{}
		for _, field := range []struct{ typ, field string }{
			{"StreamLateRefusal", "Error"}, {"StreamPromptResult", "Error"}, {"PromptRefused", "Err"}, {"Delivery", "LastSendError"},
			{"TurnOutcome", "Message"}, {"RequestTurnOutcome", "Message"},
		} {
			named, _ := s.pkg.Scope().Lookup(field.typ).(*types.TypeName)
			if named == nil {
				t.Fatalf("the package declares no type %s, whose %s this test reads as a refusal's words", field.typ, field.field)
			}
			object, _, _ := types.LookupFieldOrMethod(named.Type(), false, s.pkg, field.field)
			if object == nil {
				t.Fatalf("%s has no field %s, which this test reads as a refusal's words", field.typ, field.field)
			}
			fields[object] = true
		}
		taint.fields = func(field *types.Var) bool { return fields[field] }
		taint.methods = []string{"RefusalReason", "LastRefusal"}
	} else {
		taint.fields = func(field *types.Var) bool {
			return stringKind(field.Type()) && slices.Contains([]string{"Error", "Reason", "Message"}, field.Name())
		}
		taint.methods = []string{"Reason", "RefusalReason"}
	}
	taint.spread()
	return taint
}

// spread taints every variable that holds a text (carriesText) and that the package gives an
// expression the words reach (assignments), every field of a text that a function gives such an
// expression within it (fieldAssignments), and marks each variable of type error it gives the
// error a prompt send answers (sendAnswer), until no more is.
func (r *refusalTaint) spread() {
	given, fieldsGiven := r.src.assignments(), r.src.fieldAssignments()
	for changed := true; changed; {
		changed = false
		for target, exprs := range given {
			for _, expr := range exprs {
				if !r.tainted[target] && carriesText(target.Type()) && r.reaches(expr) {
					r.tainted[target], changed = true, true
				}
				if !r.answered[target] && r.sendAnswer(target, expr) {
					r.answered[target], changed = true, true
				}
			}
		}
		for target, exprs := range fieldsGiven {
			if r.taintedFields[target] || !carriesText(target.field.Type()) {
				continue
			}
			if slices.ContainsFunc(exprs, r.reaches) {
				r.taintedFields[target], changed = true, true
			}
		}
	}
}

// carriesText is whether a variable of typ holds a refusal's words as its value rather than in a
// field: a string of any type, bytes or runes, a slice, an array or a map of texts, a pointer to
// a text, or an error, whose Error method returns them. A struct built around the words holds
// every other field beside them — the end a failure records (EndedWork) holds its cause, which
// can quote the provider, and the kinds of work a report named — so the test follows the words
// into the field that holds them and no other, within the function that writes it
// (fieldAssignments), or into a field refusalWords names. An interface other than error, such as
// an Event, holds any of its kinds.
func carriesText(typ types.Type) bool {
	if types.Identical(typ, errorType) || stringKind(typ) {
		return true
	}
	switch under := typ.Underlying().(type) {
	case *types.Slice:
		elem, ok := under.Elem().Underlying().(*types.Basic)
		return ok && (elem.Kind() == types.Byte || elem.Kind() == types.Rune) || carriesText(under.Elem())
	case *types.Array:
		return carriesText(under.Elem())
	case *types.Map:
		return carriesText(under.Key()) || carriesText(under.Elem())
	case *types.Pointer:
		return carriesText(under.Elem())
	}
	return false
}

// errorType is the predeclared error interface.
var errorType = types.Universe.Lookup("error").Type()

// sendAnswer is whether expr gives target the error a prompt send answers, from the call itself
// or from another variable that holds it. A prompt send is a call of a method of runtime.Conn,
// the connection the supervisor sends a prompt or a compaction through, or of the stream's Conn,
// which implements it (sends): the stream's Prompt sends its frame through call, and call and
// callWithin, which Compact sends through, answer the far side's refusal as a
// stream.RefusedError. The test counts every method of either type, not Prompt alone: a
// compaction's refusal carries Oh My Pi's words too, and an error that carries none has none to
// match. In the supervisor it is the variable the send goroutine also gives its adoption's error
// (Machine.startSend). A refusal carries the agent's words in that error (runtime.RefusalReason),
// and its Error method returns them (reaches). Only a target of type error holds it: of a call
// with several results, the others, such as get_state's answer beside its error, carry no
// refusal; and a value built around the error, such as the PromptRefused the send goroutine
// hands on, holds it in a field, which the test follows where refusalWords names the field and
// nowhere else.
func (r *refusalTaint) sendAnswer(target types.Object, expr ast.Expr) bool {
	if !types.Identical(target.Type(), errorType) {
		return false
	}
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return r.answered[r.src.info.Uses[expr]]
	case *ast.CallExpr:
		return sends(r.src.callee(expr))
	}
	return false
}

// sends is whether a call of fn is a prompt send (sendAnswer): fn is a method of runtime.Conn or
// of the stream's Conn, whose methods take a pointer.
func sends(fn *types.Func) bool {
	if fn == nil || fn.Signature().Recv() == nil {
		return false
	}
	receiver := fn.Signature().Recv().Type()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	return isNamed(receiver, runtimePath, "Conn") || isNamed(receiver, streamPath, "Conn")
}

// reaches is whether a refusal's words reach expr: a field that holds them (refusalWords), a
// variable they reach, the field of a variable that holds them (fieldAssignments), a call of a
// method or function that returns them, or a call of the Error method of an error a prompt send
// answered (sendAnswer). The words in the arguments of a call that mints a member (mints) do not
// reach what it returns.
func (r *refusalTaint) reaches(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			if selection := r.src.info.Selections[node]; selection != nil && selection.Kind() == types.FieldVal {
				if field, ok := selection.Obj().(*types.Var); ok && r.fields(field) {
					found = true
				}
				if x, ok := ast.Unparen(node.X).(*ast.Ident); ok && r.taintedFields[fieldOf{r.src.info.Uses[x], selection.Obj()}] {
					found = true
				}
			}
		case *ast.Ident:
			if r.tainted[r.src.info.Uses[node]] {
				found = true
			}
		case *ast.CallExpr:
			fn := r.src.callee(node)
			if fn != nil && slices.Contains(r.methods, fn.Name()) {
				found = true
			}
			if selector, ok := ast.Unparen(node.Fun).(*ast.SelectorExpr); ok && fn != nil && fn.Name() == "Error" {
				if answer, ok := ast.Unparen(selector.X).(*ast.Ident); ok && r.answered[r.src.info.Uses[answer]] {
					found = true
				}
			}
			if !found && r.src.mints(node) {
				return false
			}
		}
		return !found
	})
	return found
}

// mints is whether call mints a member: it calls a function, not a conversion or a builtin, whose
// result is of a named type whose underlying type is string, such as runtime.RefusalOutcome or
// stream.PromptRefusalOutcome. The member is the function's answer, which the stream mints from
// the frame's code and the supervisor reads from a send's error (sendOutcome), so a site compares
// it by design (matches), whatever words the call's arguments carry. A function that returns the
// words converted to such a type is a helper, whose result the test does not follow.
func (s *goSource) mints(call *ast.CallExpr) bool {
	if fun := s.info.Types[call.Fun]; fun.IsType() || fun.IsBuiltin() {
		return false
	}
	typ := s.info.Types[call].Type
	_, named := types.Unalias(typ).(*types.Named)
	return named && stringKind(typ)
}

// stringKind is whether typ's underlying type is string: a plain string, or a type declared on
// one, such as runtime.Outcome.
func stringKind(typ types.Type) bool {
	basic, ok := typ.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// wordMatch is one place the source matches a refusal's words or holds the busy words: at, in the
// declaration in, and what the match is.
type wordMatch struct {
	at       ast.Node
	in, what string
}

// refusalMatches is every match of a refusal's words in the package's source (matches), in the
// order of its files, each with the declaration it stands in.
func (s *goSource) refusalMatches(t *testing.T) []wordMatch {
	t.Helper()
	taint := s.refusalWords(t)
	var found []wordMatch
	for _, file := range s.files {
		for _, decl := range file.Decls {
			for _, match := range taint.matches(decl) {
				match.in = s.declName(decl)
				found = append(found, match)
			}
		}
	}
	return found
}

// textMatchers are the functions of packages strings and bytes that match one text against
// another, or split it by another: what a site learns from one of them about a refusal's words is
// a reading of them. TrimPrefix and TrimSuffix are none: a trimmed text is still the words, so a
// site that compares one, with the text it was trimmed from or with anything else, compares the
// words (matches), and a site that only trims them changes what a log line shows.
var textMatchers = []string{
	"Compare", "Contains", "ContainsAny", "ContainsFunc", "ContainsRune", "Count", "Cut", "CutPrefix",
	"CutSuffix", "Equal", "EqualFold", "Fields", "FieldsFunc", "FieldsFuncSeq", "FieldsSeq", "HasPrefix",
	"HasSuffix", "Index", "IndexAny", "IndexByte", "IndexFunc", "IndexRune", "LastIndex", "LastIndexAny",
	"LastIndexByte", "LastIndexFunc", "Lines", "Split", "SplitAfter", "SplitAfterN", "SplitAfterSeq",
	"SplitN", "SplitSeq",
}

// elementMatchers are the functions of packages slices, maps, cmp and sort that compare one value
// with another, by package: whether a list holds a text, where, whether two lists or maps are
// alike, and which of two values orders first. They compare values of any type, so one compares a
// refusal's words only where it is handed them as a text or among a list's or a map's texts
// (holdsText).
var elementMatchers = map[string][]string{
	"slices": {"BinarySearch", "BinarySearchFunc", "Compare", "CompareFunc", "Contains", "ContainsFunc", "Equal", "EqualFunc", "Index", "IndexFunc"},
	"maps":   {"Equal", "EqualFunc"},
	"cmp":    {"Compare", "Less"},
	"sort":   {"SearchStrings"},
}

// textMatcher is whether fn matches one text against another: one of textMatchers in package
// strings or package bytes, package regexp's Match functions, a method of a regexp.Regexp that
// matches a text, finds in it or splits it, the Match of package path or path/filepath, which
// matches a text against a pattern, or fmt's Sscan, Sscanf or Sscanln, which read a text as the
// values it spells. errors.Is and errors.As are none: they compare an error with another by
// identity or by type, which reads none of its words, as sendOutcome reads the refusal a send
// returned.
func textMatcher(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil {
		return false
	}
	method := fn.Signature().Recv() != nil
	switch fn.Pkg().Path() {
	case "strings", "bytes":
		return !method && slices.Contains(textMatchers, fn.Name())
	case "regexp":
		return strings.HasPrefix(fn.Name(), "Match") || method && (strings.HasPrefix(fn.Name(), "Find") || fn.Name() == "Split")
	case "path", "path/filepath":
		return !method && fn.Name() == "Match"
	case "fmt":
		return !method && strings.HasPrefix(fn.Name(), "Sscan")
	}
	return false
}

// elementMatcher is whether fn is one of elementMatchers.
func elementMatcher(fn *types.Func) bool {
	return fn != nil && fn.Pkg() != nil && fn.Signature().Recv() == nil && slices.Contains(elementMatchers[fn.Pkg().Path()], fn.Name())
}

// holdsText is whether a value of typ is a text or holds texts: a value whose underlying type is
// string, or a slice, an array or a map whose elements or keys hold one. A text is any string for
// the reason a comparison's is (matches).
func holdsText(typ types.Type) bool {
	if stringKind(typ) {
		return true
	}
	switch under := typ.Underlying().(type) {
	case *types.Slice:
		return holdsText(under.Elem())
	case *types.Array:
		return holdsText(under.Elem())
	case *types.Map:
		return holdsText(under.Key()) || holdsText(under.Elem())
	}
	return false
}

// matches is every place in node that matches the words: a call of a text matcher (textMatcher)
// with them among its arguments or as its receiver; a call of an element matcher
// (elementMatcher) with them among its arguments as a text or among a list's or a map's texts;
// a lookup in a map keyed by a text they reach, alone or comma-ok; and an ==, a !=, an ordered
// comparison (<, <=, > or >=), or a switch's tag against a case, that compares a text they reach
// with anything but "" or nil. Asking whether there are words at all reads none of them. A text is
// a value whose underlying type is string: a conversion of the words to a named type, such as
// runtime.Outcome(refusal.Error) compared with a member, is the words under another name, and so
// is a variable given one. A member a call minted (mints), even from a tainted error, is the
// minter's answer, which the words do not reach, and an error compared with == is compared by
// identity, which reads none of its words.
func (r *refusalTaint) matches(node ast.Node) []wordMatch {
	text := func(expr ast.Expr) bool {
		typ := r.src.info.Types[expr].Type
		return typ != nil && stringKind(typ) && r.reaches(expr)
	}
	texts := func(expr ast.Expr) bool {
		typ := r.src.info.Types[expr].Type
		return typ != nil && holdsText(typ) && r.reaches(expr)
	}
	empty := func(expr ast.Expr) bool {
		tv := r.src.info.Types[expr]
		return tv.IsNil() || tv.Value != nil && tv.Value.Kind() == constant.String && constant.StringVal(tv.Value) == ""
	}
	compares := func(a, b ast.Expr) bool { return text(a) && !empty(b) || text(b) && !empty(a) }
	var found []wordMatch
	ast.Inspect(node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			fn := r.src.callee(node)
			reached := r.reaches
			switch {
			case textMatcher(fn):
			case elementMatcher(fn):
				reached = texts
			default:
				return true
			}
			operands := slices.Clone(node.Args)
			if selector, ok := ast.Unparen(node.Fun).(*ast.SelectorExpr); ok && r.src.info.Selections[selector] != nil {
				operands = append(operands, selector.X)
			}
			if slices.ContainsFunc(operands, reached) {
				found = append(found, wordMatch{at: node, what: "a call of " + fn.FullName()})
			}
		case *ast.IndexExpr:
			if typ := r.src.info.Types[node.X].Type; typ != nil {
				if _, isMap := typ.Underlying().(*types.Map); isMap && text(node.Index) {
					found = append(found, wordMatch{at: node, what: "a map lookup keyed by a text"})
				}
			}
		case *ast.BinaryExpr:
			switch node.Op {
			case gotoken.EQL, gotoken.NEQ, gotoken.LSS, gotoken.LEQ, gotoken.GTR, gotoken.GEQ:
				if compares(node.X, node.Y) {
					found = append(found, wordMatch{at: node, what: "a comparison with " + node.Op.String()})
				}
			}
		case *ast.SwitchStmt:
			if node.Tag == nil {
				return true
			}
			tagText, tagEmpty := text(node.Tag), empty(node.Tag)
			for _, stmt := range node.Body.List {
				if slices.ContainsFunc(stmt.(*ast.CaseClause).List, func(c ast.Expr) bool { return tagText && !empty(c) || text(c) && !tagEmpty }) {
					found = append(found, wordMatch{at: node, what: "a switch"})
					break
				}
			}
		}
		return true
	})
	return found
}
