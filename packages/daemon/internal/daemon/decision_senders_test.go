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

// senderPackages are the two packages that hold senders of a person's design decision: the
// workflow, which writes the notices and renders the resume and the kept-decisions tasks, and the
// daemon, which sends a held task at each send (rootTask) and relays a decision nobody in its tree
// hears (parkDecision).
var senderPackages = []struct{ name, dir string }{
	{"workflow", filepath.Join("..", "workflow")},
	{"daemon", "."},
}

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
// A sender is a function of internal/workflow or internal/daemon, outside its tests, whose body
//
//   - writes a record.Notice whose kind is in record.SpecDecision's set, which is read from
//     SpecDecision's own switch and checked against the function, so a kind added there is
//     covered here;
//   - asks record.SpecDecision of a notice's kind, to send the decision it names on (the outbox's
//     parkDecision relays it to the controller);
//   - holds a review sentence (decisionReviewSentences) in a string constant, which is rendering
//     the task that says it; or
//   - calls such a renderer that does not ask the rule itself: the renderer is then a template
//     (KeptDecisionsTask, the one wording a start and a held task share), its callers are its
//     senders, and a template nothing calls is a sender itself.
//
// A sender passes when it calls classify.ReviewSuperseded, or directly calls a function of the two
// packages that does: the one renderer or predicate that holds the rule for it. Calls are read as
// go/parser sees them, by name: x.f(...) reaches every method f the two packages declare, and
// pkg.f(...) the function f of a package read here.
//
// This test reads its sites from the source because no chokepoint can hold the rule: the notice
// kinds are a list a test can range over, but the review sentences are built in several functions
// and no single constructor can hold the rule, so this test stands until one can; delete it then.
func TestDecisionSendersAskWhetherTheReviewIsStillThePersonsWord(t *testing.T) {
	walk := readDecisionWalk(t)
	kinds := walk.specDecisionKinds(t)
	for _, kind := range kinds {
		if _, ok := record.SpecDecision(record.NoticeKind(kind)); !ok {
			t.Errorf("record.SpecDecision's switch names %q, but SpecDecision(%q) is no decision: the walk read the set wrongly", kind, kind)
		}
		if !slices.ContainsFunc(walk.funcs, func(f *decisionFunc) bool { return slices.Contains(f.kinds, kind) }) {
			t.Errorf("no function of internal/workflow or internal/daemon writes a record.Notice of kind %q: the walk no longer sees where that "+
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
	var out []*decisionFunc
	for _, c := range f.calls {
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

// readDecisionWalk parses every non-test file of internal/workflow and internal/daemon, and of
// internal/record for its constants and SpecDecision's switch.
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
				f := readDecisionFunc(files, pkg.name, fn, decisionReader{consts: w.consts, pkg: senderModule + pkg.name, imports: imports})
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

// readDecisionFunc reads what fn calls, the record.Notice literals it writes and the review
// sentences its string constants hold.
func readDecisionFunc(files *token.FileSet, pkg string, fn *ast.FuncDecl, reader decisionReader) *decisionFunc {
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
		case *ast.CallExpr:
			switch fun := node.Fun.(type) {
			case *ast.Ident:
				f.calls = append(f.calls, decisionCall{pkg: pkg, name: fun.Name})
			case *ast.SelectorExpr:
				x, ok := fun.X.(*ast.Ident)
				path, imported := "", false
				if ok {
					path, imported = reader.imports[x.Name]
				}
				switch {
				case !imported:
					f.calls = append(f.calls, decisionCall{name: fun.Sel.Name, method: true})
				case x.Name == classify && fun.Sel.Name == "ReviewSuperseded":
					f.asksRule = true
				case x.Name == recordPkg && fun.Sel.Name == "SpecDecision":
					f.picks = true
				case strings.HasPrefix(path, senderModule):
					f.calls = append(f.calls, decisionCall{pkg: strings.TrimPrefix(path, senderModule), name: fun.Sel.Name})
				}
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
