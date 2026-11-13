package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// senderModule is the import path every package the walk reads lives under.
const senderModule = "github.com/sjawhar/legion/daemon/internal/"

// senderPackages are the three packages that hold senders of a person's design decision: the
// workflow, which writes the notices and renders the resume and the kept-decisions tasks, the
// daemon, which sends a held task at each send (rootTask) and relays a decision nobody in its tree
// hears (parkDecision), and admission, which composes a re-admitted root's start from what was
// kept for it.
var senderPackages = []struct{ name, dir string }{
	{"workflow", filepath.Join("..", "workflow")},
	{"daemon", "."},
	{"admit", filepath.Join("..", "admit")},
}

// keptDecisionTypes are the record types that hold one kept decision: the row the daemon keeps
// (record.ParkedDecision) and the mark a task names it by (record.DecisionMark). A slice of
// either is a list of kept decisions, and so are a record function's result of that type and a
// record field of it (record.KeptDecisions.Decisions), which the walk reads from record's source.
var keptDecisionTypes = []string{"ParkedDecision", "DecisionMark"}

// decisionReviewSentences is every fixed phrase a task says about a person's review of a design
// document, each with the function that renders it. A function whose string constants hold one
// renders that review: it is a sender, or the template its callers send (decisionSenders). The
// test holds each named function to its phrase, so a phrase reworded out of the list fails here
// rather than leaving its renderer unread.
var decisionReviewSentences = []struct{ phrase, renderer string }{
	// The changes request that answered the wait.
	{"A person requested changes to your", "workflow.resumeTask"},
	// That changes request, when the person's approval followed it.
	{"asks for no revision now", "workflow.resumeTask"},
	// A changes request the gate records as told after the wait began.
	{"A person then requested changes to your", "workflow.resumeTask"},
	// The decisions kept while nobody in the tree could hear them.
	{"a person decided on the tree", "workflow.KeptDecisionsTask"},
	// The kept approval of a version the document has moved past.
	{"and is now at version", "workflow.KeptDecisionsTask"},
}

// Every sender of a person's design decision asks the one rule whether that decision is still the
// person's word (classify.ReviewSuperseded), so no sender decides it for itself
// (dispatch://INFRA-3136/spec R2).
// A sender is a function of internal/workflow, internal/daemon or internal/admit, outside its
// tests, whose body
//
//   - writes a record.Notice whose kind is in record.SpecDecision's set, which is read from
//     SpecDecision's own switch and checked against the function, so a kind added there is
//     covered here;
//   - asks record.SpecDecision of a notice's kind, to send the decision it names on (the outbox's
//     parkDecision relays it to the controller);
//   - holds a review sentence (decisionReviewSentences) in a string constant, which is rendering
//     the task that says it; or
//   - calls such a renderer that does not ask the rule itself: the renderer is then a template,
//     its callers are its senders, and a template nothing calls is a sender itself.
//
// A sender passes when it calls classify.ReviewSuperseded, or directly calls a function of the
// three packages that does: the one renderer or predicate that holds the rule for it. Calls are
// read as go/parser sees them, by name: x.f(...) reaches every method f the three packages
// declare, and pkg.f(...) the function f of a package read here.
//
// A list of kept decisions is held to more: every loop that ranges over one (keptDecisionTypes)
// and writes a decision's kind into text (a fmt print, a WriteString, a string conversion) is a
// renderer of kept decisions. Its body, the part that runs once per decision, has to call the
// rule or a function that does. A function that asks the rule about one decision, or about the
// kept approval, and renders the rest of the list unasked is what this test fails on: a task
// named a changes request that the person's approval had superseded that way.
//
// This test reads its sites from the source because no chokepoint can hold the rule: the notice
// kinds are a list a test can range over, but the review sentences are built in several functions
// and no single constructor can hold the rule, and a list of kept decisions is a plain slice that
// any function can range over and write into text, which no type can make ask first. So this
// test stands until one can; delete it then.
func TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord(t *testing.T) {
	walk := readDecisionWalk(t)
	kinds := walk.specDecisionKinds(t)
	for _, kind := range kinds {
		if _, ok := record.SpecDecision(record.NoticeKind(kind)); !ok {
			t.Errorf("record.SpecDecision's switch names %q, but SpecDecision(%q) is no decision: the walk read the set wrongly", kind, kind)
		}
		if !slices.ContainsFunc(walk.funcs, func(f *decisionFunc) bool { return slices.Contains(f.kinds, kind) }) {
			t.Errorf("no function of internal/workflow, internal/daemon or internal/admit writes a record.Notice of kind %q: the walk no longer sees where that "+
				"decision is sent, so it proves nothing of its sender", kind)
		}
	}
	for _, sentence := range decisionReviewSentences {
		f := walk.named(sentence.renderer)
		if f == nil || !slices.Contains(f.phrases, sentence.phrase) {
			t.Errorf("%s no longer holds the review sentence %q: name the phrase it renders now in decisionReviewSentences", sentence.renderer, sentence.phrase)
		}
	}
	senders := walk.decisionSenders()
	for _, s := range senders {
		t.Logf("sender %s (%s): %s", s.fn.qualified(), s.fn.at, s.why)
		if !walk.asksTheRule(s.fn) {
			t.Errorf("%s (%s) %s, but neither calls classify.ReviewSuperseded nor calls a function that does: it decides for itself whether "+
				"the person's review is still their word", s.fn.qualified(), s.fn.at, s.why)
		}
	}
	for _, f := range walk.funcs {
		for _, at := range f.unread {
			t.Errorf("%s writes a record.Notice at %s whose kind the walk cannot read: give it a constant kind, or a local assigned only "+
				"constants, so the walk can tell whether it sends a person's decision", f.qualified(), at)
		}
	}
	for _, f := range walk.funcs {
		for _, loop := range f.keptLoops {
			if !loop.asks && !slices.ContainsFunc(walk.calledFuncs(loop.calls), func(g *decisionFunc) bool { return g.asksRule }) {
				t.Errorf("%s (%s) ranges over kept decisions at %s and writes each one into text, but asks the rule "+
					"(classify.ReviewSuperseded, directly or through a function that calls it) about none of them: it names a decision "+
					"without knowing whether a later approval superseded it", f.qualified(), f.at, loop.at)
			}
		}
	}
	// The one wording of kept decisions (workflow.KeptDecisionsTask) renders them in a loop: the
	// walk has to see it, or it proves nothing of any renderer.
	if f := walk.named("workflow.KeptDecisionsTask"); f == nil || len(f.keptLoops) == 0 {
		t.Error("the walk found no loop over kept decisions that writes them into text in workflow.KeptDecisionsTask")
	}
	// The resume of an answered root is rendered at each send from the gate as it stands
	// (workflow.ResumeTask, the daemon's rootTask), so it is a sender too: the walk has to see it.
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.qualified() == "workflow.resumeTask" }) {
		t.Error("the walk found no resume sender (workflow.resumeTask) in internal/workflow")
	}
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.pkg == "daemon" }) {
		t.Error("the walk found no sender in internal/daemon")
	}
}

// decisionFunc is what the walk reads of one function declaration, its function literals
// included: they run as part of it (a transaction's body is one).
type decisionFunc struct {
	pkg, name string // name is Recv.name for a method
	at        string // file:line of the declaration
	asksRule  bool   // calls classify.ReviewSuperseded
	picks     bool   // calls record.SpecDecision
	kinds     []string
	unread    []string // positions of record.Notice literals whose kind the walk cannot read
	phrases   []string
	calls     []decisionCall
	keptLoops []keptLoop
}

// keptLoop is a loop of a function over a list of kept decisions that writes a decision's kind
// into text: where it is, and what its body calls once per decision.
type keptLoop struct {
	at    string
	asks  bool // the body calls classify.ReviewSuperseded
	calls []decisionCall
}

func (f *decisionFunc) qualified() string { return f.pkg + "." + f.name }

// decisionCall is a call as the walk names it: a function of pkg, or, for method, a method by name.
type decisionCall struct {
	pkg, name string
	method    bool
}

type decisionSender struct {
	fn  *decisionFunc
	why string
}

type decisionWalk struct {
	funcs   []*decisionFunc
	plain   map[string]*decisionFunc   // pkg.name → function
	methods map[string][]*decisionFunc // method name → every method of that name
	consts  map[string]map[string]string
	record  []*ast.File
	kept    keptShapes
}

// keptShapes is how a list of kept decisions is spelled, read from record's source: the names of
// the record functions and methods whose first result is one, and of the record fields that hold
// one.
type keptShapes struct {
	results, fields map[string]bool
}

func (w *decisionWalk) named(qualified string) *decisionFunc {
	for _, f := range w.funcs {
		if f.qualified() == qualified {
			return f
		}
	}
	return nil
}

func (w *decisionWalk) callees(f *decisionFunc) []*decisionFunc {
	return w.calledFuncs(f.calls)
}

// calledFuncs is every function of the walk that calls can reach.
func (w *decisionWalk) calledFuncs(calls []decisionCall) []*decisionFunc {
	var out []*decisionFunc
	for _, c := range calls {
		if c.method {
			out = append(out, w.methods[c.name]...)
		} else if g := w.plain[c.pkg+"."+c.name]; g != nil {
			out = append(out, g)
		}
	}
	return out
}

// asksTheRule is whether f calls classify.ReviewSuperseded, or calls a function that does.
func (w *decisionWalk) asksTheRule(f *decisionFunc) bool {
	return f.asksRule || slices.ContainsFunc(w.callees(f), func(g *decisionFunc) bool { return g.asksRule })
}

// decisionSenders is every sender the walk finds (the test's doc comment), each once, with what
// makes it one.
func (w *decisionWalk) decisionSenders() []decisionSender {
	var senders []decisionSender
	add := func(f *decisionFunc, why string) {
		for i := range senders {
			if senders[i].fn == f {
				senders[i].why += "; " + why
				return
			}
		}
		senders = append(senders, decisionSender{f, why})
	}
	for _, f := range w.funcs {
		if len(f.kinds) > 0 {
			add(f, fmt.Sprintf("writes a record.Notice of kind %s", strings.Join(f.kinds, ", ")))
		}
		if f.picks {
			add(f, "asks record.SpecDecision of a notice's kind and sends the decision it names on")
		}
		if len(f.phrases) == 0 {
			continue
		}
		if w.asksTheRule(f) {
			add(f, fmt.Sprintf("renders %q", f.phrases))
			continue
		}
		var callers []*decisionFunc
		for _, g := range w.funcs {
			if g != f && slices.Contains(w.callees(g), f) {
				callers = append(callers, g)
			}
		}
		if len(callers) == 0 {
			add(f, fmt.Sprintf("renders %q, and nothing calls it to ask the rule for it", f.phrases))
		}
		for _, g := range callers {
			add(g, fmt.Sprintf("sends what %s renders, %q", f.qualified(), f.phrases))
		}
	}
	return senders
}

// specDecisionKinds is record.SpecDecision's set as its switch spells it: every case value.
func (w *decisionWalk) specDecisionKinds(t *testing.T) []string {
	t.Helper()
	reader := decisionReader{consts: w.consts, pkg: senderModule + "record"}
	var kinds []string
	for _, file := range w.record {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "SpecDecision" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				clause, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, expr := range clause.List {
					values, ok := reader.values(expr, 0)
					if !ok {
						t.Fatalf("record.SpecDecision has a case the walk cannot read: %#v", expr)
					}
					kinds = append(kinds, values...)
				}
				return true
			})
		}
	}
	if len(kinds) == 0 {
		t.Fatal("read no kind from record.SpecDecision's switch")
	}
	return kinds
}

// readDecisionWalk parses every non-test file of the sender packages, and of internal/record for
// its constants, SpecDecision's switch and how a list of kept decisions is spelled (keptShapes).
func readDecisionWalk(t *testing.T) *decisionWalk {
	t.Helper()
	files := token.NewFileSet()
	parse := func(dir string) []*ast.File {
		paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		var parsed []*ast.File
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			parsed = append(parsed, file)
		}
		if len(parsed) == 0 {
			t.Fatalf("read no Go file in %s", dir)
		}
		return parsed
	}
	w := &decisionWalk{plain: map[string]*decisionFunc{}, methods: map[string][]*decisionFunc{}, consts: map[string]map[string]string{}}
	w.record = parse(filepath.Join("..", "record"))
	w.consts[senderModule+"record"] = fileConsts(w.record)
	w.kept = readKeptShapes(t, w.record)
	parsed := map[string][]*ast.File{}
	for _, pkg := range senderPackages {
		parsed[pkg.name] = parse(pkg.dir)
		w.consts[senderModule+pkg.name] = fileConsts(parsed[pkg.name])
	}
	for _, pkg := range senderPackages {
		for _, file := range parsed[pkg.name] {
			imports := fileImports(file)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				f := readDecisionFunc(files, pkg.name, fn, decisionReader{consts: w.consts, pkg: senderModule + pkg.name, imports: imports}, w.kept)
				w.funcs = append(w.funcs, f)
				if fn.Recv == nil {
					w.plain[f.qualified()] = f
				} else {
					w.methods[fn.Name.Name] = append(w.methods[fn.Name.Name], f)
				}
			}
		}
	}
	return w
}

// readDecisionFunc reads what fn calls, the record.Notice literals it writes, the review
// sentences its string constants hold and its loops over kept decisions that write them as text.
func readDecisionFunc(files *token.FileSet, pkg string, fn *ast.FuncDecl, reader decisionReader, kept keptShapes) *decisionFunc {
	f := &decisionFunc{pkg: pkg, name: fn.Name.Name, at: files.Position(fn.Pos()).String()}
	if fn.Recv != nil {
		f.name = receiverName(fn.Recv.List[0].Type) + "." + f.name
	}
	reader.locals = localAssignments(fn.Body)
	classify, recordPkg := reader.importName(senderModule+"classify"), reader.importName(senderModule+"record")
	var noticeLit func(lit *ast.CompositeLit)
	noticeLit = func(lit *ast.CompositeLit) {
		for _, elt := range lit.Elts {
			keyed, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				f.unread = append(f.unread, files.Position(lit.Pos()).String())
				return
			}
			if key, ok := keyed.Key.(*ast.Ident); ok && key.Name == "Kind" {
				values, ok := reader.values(keyed.Value, 0)
				if !ok {
					f.unread = append(f.unread, files.Position(lit.Pos()).String())
					return
				}
				for _, value := range values {
					if _, spec := record.SpecDecision(record.NoticeKind(value)); spec && !slices.Contains(f.kinds, value) {
						f.kinds = append(f.kinds, value)
					}
				}
			}
		}
	}
	isNotice := func(expr ast.Expr) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := selector.X.(*ast.Ident)
		return ok && recordPkg != "" && x.Name == recordPkg && selector.Sel.Name == "Notice"
	}
	var holds func(text string)
	holds = func(text string) {
		for _, sentence := range decisionReviewSentences {
			if strings.Contains(text, sentence.phrase) && !slices.Contains(f.phrases, sentence.phrase) {
				f.phrases = append(f.phrases, sentence.phrase)
			}
		}
	}
	// callOf names a call as the walk reads it: asks for a call of classify.ReviewSuperseded,
	// picks for one of record.SpecDecision, and named for a call of a function of a package the
	// walk reads, or of a method by its name.
	callOf := func(node *ast.CallExpr) (call decisionCall, asks, picks, named bool) {
		switch fun := node.Fun.(type) {
		case *ast.Ident:
			return decisionCall{pkg: pkg, name: fun.Name}, false, false, true
		case *ast.SelectorExpr:
			x, ok := fun.X.(*ast.Ident)
			path, imported := "", false
			if ok {
				path, imported = reader.imports[x.Name]
			}
			switch {
			case !imported:
				return decisionCall{name: fun.Sel.Name, method: true}, false, false, true
			case x.Name == classify && fun.Sel.Name == "ReviewSuperseded":
				return decisionCall{}, true, false, false
			case x.Name == recordPkg && fun.Sel.Name == "SpecDecision":
				return decisionCall{}, false, true, false
			case strings.HasPrefix(path, senderModule):
				return decisionCall{pkg: strings.TrimPrefix(path, senderModule), name: fun.Sel.Name}, false, false, true
			}
		}
		return decisionCall{}, false, false, false
	}
	lists := keptLocals(fn, recordPkg, kept)
	var keptList func(expr ast.Expr) bool
	keptList = func(expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.ParenExpr:
			return keptList(e.X)
		case *ast.Ident:
			return lists[e.Name]
		case *ast.SelectorExpr:
			return kept.fields[e.Sel.Name]
		case *ast.CompositeLit:
			return isKeptList(e.Type, recordPkg)
		}
		return false
	}
	fmtPkg := reader.importName("fmt")
	// writesText is whether call writes its arguments into text: a print of fmt (an error's text,
	// fmt.Errorf, reaches no person), a WriteString, or a conversion to string.
	writesText := func(call *ast.CallExpr) bool {
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			return fun.Name == "string"
		case *ast.SelectorExpr:
			if fun.Sel.Name == "WriteString" {
				return true
			}
			x, ok := fun.X.(*ast.Ident)
			if !ok || fmtPkg == "" || x.Name != fmtPkg {
				return false
			}
			return slices.ContainsFunc([]string{"Sprint", "Fprint", "Append"}, func(prefix string) bool { return strings.HasPrefix(fun.Sel.Name, prefix) })
		}
		return false
	}
	// keptLoopOf reads loop when it ranges over a list of kept decisions and writes a decision's
	// kind (the field Decision of the element, by the loop's value or by its index) into text.
	keptLoopOf := func(loop *ast.RangeStmt) (keptLoop, bool) {
		if !keptList(loop.X) {
			return keptLoop{}, false
		}
		value, index := identName(loop.Value), identName(loop.Key)
		names := func(expr ast.Expr) bool {
			found := false
			ast.Inspect(expr, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Decision" {
					switch x := sel.X.(type) {
					case *ast.Ident:
						found = found || value != "" && x.Name == value
					case *ast.IndexExpr:
						found = found || index != "" && identName(x.Index) == index
					}
				}
				return !found
			})
			return found
		}
		read := keptLoop{at: files.Position(loop.Pos()).String()}
		writes := false
		ast.Inspect(loop.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if writesText(call) && slices.ContainsFunc(call.Args, names) {
				writes = true
			}
			if named, asks, _, ok := callOf(call); asks {
				read.asks = true
			} else if ok {
				read.calls = append(read.calls, named)
			}
			return true
		})
		return read, writes
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr, *ast.BasicLit:
			if text, ok := literalString(node.(ast.Expr)); ok {
				holds(text)
				return false
			}
		case *ast.Ident:
			if text, ok := reader.consts[reader.pkg][node.Name]; ok {
				holds(text)
			}
		case *ast.CompositeLit:
			switch {
			case isNotice(node.Type):
				noticeLit(node)
			case node.Type != nil:
				// A slice or map of notices elides the type of each literal it holds.
				var elt ast.Expr
				switch typ := node.Type.(type) {
				case *ast.ArrayType:
					elt = typ.Elt
				case *ast.MapType:
					elt = typ.Value
				}
				if elt != nil && isNotice(elt) {
					for _, e := range node.Elts {
						if keyed, ok := e.(*ast.KeyValueExpr); ok {
							e = keyed.Value
						}
						if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
							noticeLit(inner)
						}
					}
				}
			}
		case *ast.RangeStmt:
			if loop, ok := keptLoopOf(node); ok {
				f.keptLoops = append(f.keptLoops, loop)
			}
		case *ast.CallExpr:
			call, asks, picks, named := callOf(node)
			switch {
			case asks:
				f.asksRule = true
			case picks:
				f.picks = true
			case named:
				f.calls = append(f.calls, call)
			}
		}
		return true
	})
	return f
}

// decisionReader reads the constant values an expression can take in a function of pkg: a string
// literal, a constant of a package the walk read, a conversion of one (record.NoticeKind("x")),
// or a local variable every assignment of which is one of those.
type decisionReader struct {
	consts  map[string]map[string]string // import path → constant → value
	pkg     string
	imports map[string]string // local name → import path
	locals  map[string][]ast.Expr
}

func (r decisionReader) importName(path string) string {
	for name, imported := range r.imports {
		if imported == path {
			return name
		}
	}
	return ""
}

func (r decisionReader) values(expr ast.Expr, depth int) ([]string, bool) {
	if depth > 8 {
		return nil, false
	}
	switch e := expr.(type) {
	case *ast.BasicLit, *ast.BinaryExpr:
		text, ok := literalString(e)
		return []string{text}, ok
	case *ast.ParenExpr:
		return r.values(e.X, depth+1)
	case *ast.Ident:
		if assigned, ok := r.locals[e.Name]; ok {
			var all []string
			for _, value := range assigned {
				values, ok := r.values(value, depth+1)
				if !ok {
					return nil, false
				}
				all = append(all, values...)
			}
			return all, true
		}
		text, ok := r.consts[r.pkg][e.Name]
		return []string{text}, ok
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return nil, false
		}
		text, ok := r.consts[r.imports[x.Name]][e.Sel.Name]
		return []string{text}, ok
	case *ast.CallExpr:
		name := ""
		switch fun := e.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if name == "NoticeKind" && len(e.Args) == 1 {
			return r.values(e.Args[0], depth+1)
		}
	}
	return nil, false
}

// literalString is the string a literal, or a concatenation of literals, spells.
func literalString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(e.Value)
		return text, err == nil
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := literalString(e.X)
		if !ok {
			return "", false
		}
		right, ok := literalString(e.Y)
		return left + right, ok
	case *ast.ParenExpr:
		return literalString(e.X)
	}
	return "", false
}

// fileConsts is every package-level constant of files whose value is a string literal, or a
// conversion of one.
func fileConsts(files []*ast.File) map[string]string {
	consts := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) {
						break
					}
					expr := value.Values[i]
					if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) == 1 {
						expr = call.Args[0]
					}
					if text, ok := literalString(expr); ok {
						consts[name.Name] = text
					}
				}
			}
		}
	}
	return consts
}

func fileImports(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

// localAssignments is every value body assigns to each name it declares or assigns, function
// literals included.
func localAssignments(body *ast.BlockStmt) map[string][]ast.Expr {
	locals := map[string][]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				if len(node.Lhs) == len(node.Rhs) {
					locals[ident.Name] = append(locals[ident.Name], node.Rhs[i])
				} else {
					// A tuple assignment's values are no constants the walk can read.
					locals[ident.Name] = append(locals[ident.Name], &ast.BadExpr{})
				}
			}
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				if i < len(node.Values) {
					locals[ident.Name] = append(locals[ident.Name], node.Values[i])
				} else {
					locals[ident.Name] = append(locals[ident.Name], &ast.BadExpr{})
				}
			}
		}
		return true
	})
	return locals
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return "?"
}

// identName is the name expr spells when it is an identifier other than the blank one, else "".
func identName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok && ident.Name != "_" {
		return ident.Name
	}
	return ""
}

// isKeptList is whether typ spells a slice of a kept decision type (keptDecisionTypes): qualified
// by recordPkg, the name a file gives the record package, or bare where recordPkg is "" (record's
// own source).
func isKeptList(typ ast.Expr, recordPkg string) bool {
	slice, ok := typ.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	switch elt := slice.Elt.(type) {
	case *ast.Ident:
		return recordPkg == "" && slices.Contains(keptDecisionTypes, elt.Name)
	case *ast.SelectorExpr:
		x, ok := elt.X.(*ast.Ident)
		return ok && recordPkg != "" && x.Name == recordPkg && slices.Contains(keptDecisionTypes, elt.Sel.Name)
	}
	return false
}

// firstResultIsKeptList is whether a function type's first result is a list of kept decisions.
func firstResultIsKeptList(fn *ast.FuncType, recordPkg string) bool {
	return fn.Results != nil && len(fn.Results.List) > 0 && isKeptList(fn.Results.List[0].Type, recordPkg)
}

// readKeptShapes reads from record's source the functions and interface methods whose first
// result is a list of kept decisions and the struct fields that hold one, and checks that each
// kept decision type is still a struct with a Decision field, which is the kind a task writes.
func readKeptShapes(t *testing.T, files []*ast.File) keptShapes {
	t.Helper()
	shapes := keptShapes{results: map[string]bool{}, fields: map[string]bool{}}
	decided := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if firstResultIsKeptList(node.Type, "") {
					shapes.results[node.Name.Name] = true
				}
			case *ast.TypeSpec:
				switch typ := node.Type.(type) {
				case *ast.StructType:
					for _, field := range typ.Fields.List {
						for _, name := range field.Names {
							if isKeptList(field.Type, "") {
								shapes.fields[name.Name] = true
							}
							if kind, ok := field.Type.(*ast.Ident); ok && name.Name == "Decision" && kind.Name == "DecisionKind" {
								decided[node.Name.Name] = true
							}
						}
					}
				case *ast.InterfaceType:
					for _, method := range typ.Methods.List {
						if fn, ok := method.Type.(*ast.FuncType); ok && firstResultIsKeptList(fn, "") {
							for _, name := range method.Names {
								shapes.results[name.Name] = true
							}
						}
					}
				}
			}
			return true
		})
	}
	for _, typ := range keptDecisionTypes {
		if !decided[typ] {
			t.Fatalf("record.%s is no longer a struct with a Decision field of DecisionKind: name the kept decision types in keptDecisionTypes", typ)
		}
	}
	if len(shapes.results) == 0 || len(shapes.fields) == 0 {
		t.Fatalf("read from record no function that returns a list of kept decisions (%v) or no field that holds one (%v)", shapes.results, shapes.fields)
	}
	return shapes
}

// keptLocals is every name fn, its function literals included, gives a list of kept decisions: a
// parameter or result of that type, a variable declared with it, and a name assigned a literal or
// a make of it, or the first result of a record function that returns one (keptShapes.results).
func keptLocals(fn *ast.FuncDecl, recordPkg string, kept keptShapes) map[string]bool {
	lists := map[string]bool{}
	fields := func(list *ast.FieldList) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			if isKeptList(field.Type, recordPkg) {
				for _, name := range field.Names {
					lists[name.Name] = true
				}
			}
		}
	}
	fields(fn.Type.Params)
	fields(fn.Type.Results)
	returnsList := func(expr ast.Expr) bool {
		switch e := expr.(type) {
		case *ast.CompositeLit:
			return isKeptList(e.Type, recordPkg)
		case *ast.CallExpr:
			switch fun := e.Fun.(type) {
			case *ast.Ident:
				return fun.Name == "make" && len(e.Args) > 0 && isKeptList(e.Args[0], recordPkg)
			case *ast.SelectorExpr:
				return kept.results[fun.Sel.Name]
			}
		}
		return false
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncLit:
			fields(node.Type.Params)
			fields(node.Type.Results)
		case *ast.ValueSpec:
			if node.Type != nil && isKeptList(node.Type, recordPkg) {
				for _, name := range node.Names {
					lists[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			switch {
			case len(node.Lhs) == len(node.Rhs):
				for i, lhs := range node.Lhs {
					if name := identName(lhs); name != "" && returnsList(node.Rhs[i]) {
						lists[name] = true
					}
				}
			case len(node.Rhs) == 1:
				if name := identName(node.Lhs[0]); name != "" && returnsList(node.Rhs[0]) {
					lists[name] = true
				}
			}
		}
		return true
	})
	return lists
}
