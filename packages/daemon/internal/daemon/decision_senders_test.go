package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// senderModule is the import path every package the walk reads lives under.
const senderModule = "github.com/sjawhar/legion/daemon/internal/"

// decisionPredicate is the one predicate that decides whether a person's recorded review of a
// design document is still their word (workflow.DecisionStands), and decisionRule the rule it asks
// (classify.ReviewSuperseded), each as go/types names it (types.Func.FullName).
const (
	decisionPredicate = senderModule + "workflow.DecisionStands"
	decisionRule      = senderModule + "classify.ReviewSuperseded"
)

// ruleAskers is every function of the three packages that may ask the rule itself, with why.
var ruleAskers = map[string]string{
	decisionPredicate:                       "the one predicate, which holds every guard that chooses whether to ask it",
	senderModule + "workflow.requestStands": "an approval request on the ask mirror, which is the architect's ask and no person's decision",
}

// senderPackages are the three packages that hold senders of a person's design decision: the
// workflow, which writes the notices and renders the resume and the kept-decisions tasks, the
// daemon, which sends a held task at each send (rootTask), writes a catch-up's decisions again at
// its delivery (readNoticeTree) and relays a decision nobody in its tree hears (parkDecision), and
// admission, which composes a re-admitted root's start from what was kept for it.
var senderPackages = []string{"workflow", "daemon", "admit"}

// keptDecisionTypes are the record types that hold one kept decision: the row the daemon keeps
// (record.ParkedDecision) and the mark a task names it by (record.DecisionMark). A value whose
// type is a slice of either is a list of kept decisions, whatever spells it.
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

// Every sender of a person's design decision asks the one predicate whether that decision is still
// the person's word (workflow.DecisionStands), so no sender decides it for itself and no sender
// chooses for itself whether to ask the rule (dispatch://INFRA-3136/spec R2).
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
//   - calls such a renderer that does not reach the predicate itself: the renderer is then a
//     template, its callers are its senders, and a template nothing calls is a sender itself.
//
// A sender passes when it reaches the predicate: it calls it, or calls a function of the three
// packages that reaches it, however many calls down. The walk type-checks the three packages
// (and record, for SpecDecision's switch) against the compiler's export data of their
// dependencies (compiledDependencies) and resolves every call, and every function value a body
// names, through types.Info to the function it is, so a method that shares a name with another is
// never mistaken for it.
//
// A list of kept decisions is held to more: every loop that ranges over one (keptDecisionTypes)
// and writes a decision's kind into text (a fmt print, a WriteString, a string conversion) is a
// renderer of kept decisions. Its body, the part that runs once per decision, has to reach the
// predicate. A function that asks about one decision, or about the kept approval, and renders the
// rest of the list unasked is what this test fails on: a task named a changes request that the
// person's approval had superseded that way.
//
// And the rule itself (classify.ReviewSuperseded) is asked by the predicate alone, and by the one
// reading of an approval request (ruleAskers), so the guards that choose whether to ask it live in
// one place.
//
// This test reads its sites from the source because no chokepoint can make a sender ask: the
// predicate is the one place the answer is decided, but a notice is a plain struct literal any
// function can write, the review sentences are built in several functions, and a list of kept
// decisions is a plain slice any function can range over and write into text, and no type can
// make any of them ask the predicate first. So this test stands until one can; delete it then.
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
	if predicate := walk.byName[decisionPredicate]; predicate == nil || !predicate.asks {
		t.Fatalf("the walk found no %s asking %s: it reads the predicate under that name", short(decisionPredicate), short(decisionRule))
	}
	senders := walk.decisionSenders()
	for _, s := range senders {
		t.Logf("sender %s (%s): %s", s.fn.short(), s.fn.at, s.why)
		if !walk.reaches(s.fn.calls) {
			t.Errorf("%s (%s) %s, but does not reach %s, directly or through any function it calls: it decides for itself whether "+
				"the person's review is still their word", s.fn.short(), s.fn.at, s.why, short(decisionPredicate))
		}
	}
	for _, f := range walk.funcs {
		for _, at := range f.unread {
			t.Errorf("%s writes a record.Notice at %s whose kind the walk cannot read: give it a constant kind, or a local assigned only "+
				"constants, so the walk can tell whether it sends a person's decision", f.short(), at)
		}
		for _, loop := range f.keptLoops {
			if !walk.reaches(loop.calls) {
				t.Errorf("%s (%s) ranges over kept decisions at %s and writes each one into text, but asks %s about none of them, "+
					"directly or through a function it calls: it names a decision without knowing whether a later approval superseded it",
					f.short(), f.at, loop.at, short(decisionPredicate))
			}
		}
		if f.asks {
			if _, ok := ruleAskers[f.name]; !ok {
				t.Errorf("%s (%s) asks %s itself: ask %s, which holds every guard that chooses whether to ask the rule, so no copy of them "+
					"can drift", f.short(), f.at, short(decisionRule), short(decisionPredicate))
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
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.short() == "workflow.resumeTask" }) {
		t.Error("the walk found no resume sender (workflow.resumeTask) in internal/workflow")
	}
	if !slices.ContainsFunc(senders, func(s decisionSender) bool { return s.fn.pkg == "daemon" }) {
		t.Error("the walk found no sender in internal/daemon")
	}
}

// decisionFunc is what the walk reads of one function declaration, its function literals
// included: they run as part of it (a transaction's body is one).
type decisionFunc struct {
	pkg       string // the package's last element
	name      string // types.Func.FullName
	at        string // file:line of the declaration
	asks      bool   // names classify.ReviewSuperseded
	picks     bool   // names record.SpecDecision
	kinds     []string
	unread    []string // positions of record.Notice literals whose kind the walk cannot read
	phrases   []string
	calls     []string // the FullName of every function the body calls or names
	keptLoops []keptLoop
}

func (f *decisionFunc) short() string { return short(f.name) }

// short is a function's FullName without the module's path: workflow.DecisionStands,
// (*workflow.Engine).tellChanges.
func short(name string) string { return strings.ReplaceAll(name, senderModule, "") }

// keptLoop is a loop of a function over a list of kept decisions that writes a decision's kind
// into text: where it is, and what its body calls once per decision.
type keptLoop struct {
	at    string
	calls []string
}

type decisionSender struct {
	fn  *decisionFunc
	why string
}

type decisionWalk struct {
	funcs  []*decisionFunc
	byName map[string]*decisionFunc // FullName → function
	record *checkedPackage
}

// named is the function short names (short), nil when the walk read none.
func (w *decisionWalk) named(name string) *decisionFunc {
	for _, f := range w.funcs {
		if f.short() == name {
			return f
		}
	}
	return nil
}

// reaches is whether calls name the predicate, or a function of the walk from which a chain of
// calls within the three packages reaches it.
func (w *decisionWalk) reaches(calls []string) bool {
	seen := map[string]bool{}
	next := slices.Clone(calls)
	for len(next) > 0 {
		name := next[len(next)-1]
		next = next[:len(next)-1]
		if name == decisionPredicate {
			return true
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if f := w.byName[name]; f != nil {
			next = append(next, f.calls...)
		}
	}
	return false
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
		if w.reaches(f.calls) {
			add(f, fmt.Sprintf("renders %q", f.phrases))
			continue
		}
		var callers []*decisionFunc
		for _, g := range w.funcs {
			if g != f && slices.Contains(g.calls, f.name) {
				callers = append(callers, g)
			}
		}
		if len(callers) == 0 {
			add(f, fmt.Sprintf("renders %q, and nothing calls it to ask the predicate for it", f.phrases))
		}
		for _, g := range callers {
			add(g, fmt.Sprintf("sends what %s renders, %q", f.short(), f.phrases))
		}
	}
	return senders
}

// specDecisionKinds is record.SpecDecision's set as its switch spells it: every case value, as
// the type checker reads its constant.
func (w *decisionWalk) specDecisionKinds(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, file := range w.record.files {
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
					value := w.record.info.Types[expr].Value
					if value == nil || value.Kind() != constant.String {
						t.Fatalf("record.SpecDecision has a case the walk cannot read as a constant: %s", w.record.pos(expr))
					}
					kinds = append(kinds, constant.StringVal(value))
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

// checkedPackage is one package's non-test source, type-checked.
type checkedPackage struct {
	name  string
	files []*ast.File
	fset  *token.FileSet
	info  *types.Info
}

func (p *checkedPackage) pos(node ast.Node) string {
	position := p.fset.Position(node.Pos())
	return fmt.Sprintf("%s:%d", filepath.Join(p.name, filepath.Base(position.Filename)), position.Line)
}

// checkPackage parses the non-test files of the package in dir, as the build would select them,
// and type-checks them against the compiler's export data of their dependencies.
func checkPackage(t *testing.T, fset *token.FileSet, imports types.Importer, name string) *checkedPackage {
	t.Helper()
	dir := filepath.Join("..", name)
	listed, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("list the source of %s: %v", dir, err)
	}
	checked := &checkedPackage{name: name, fset: fset, info: &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}}
	for _, file := range listed.GoFiles {
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		checked.files = append(checked.files, parsed)
	}
	if len(checked.files) == 0 {
		t.Fatalf("read no Go file in %s", dir)
	}
	config := types.Config{Importer: imports}
	if _, err := config.Check(senderModule+name, fset, checked.files, checked.info); err != nil {
		t.Fatalf("type-check %s: %v", name, err)
	}
	return checked
}

// readDecisionWalk type-checks record and the three sender packages and reads every function
// declaration of the three.
func readDecisionWalk(t *testing.T) *decisionWalk {
	t.Helper()
	fset := token.NewFileSet()
	imports := importer.ForCompiler(fset, "gc", compiledDependencies(t))
	w := &decisionWalk{byName: map[string]*decisionFunc{}, record: checkPackage(t, fset, imports, "record")}
	for _, name := range senderPackages {
		pkg := checkPackage(t, fset, imports, name)
		for _, file := range pkg.files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				f := readDecisionFunc(pkg, fn)
				w.funcs = append(w.funcs, f)
				w.byName[f.name] = f
			}
		}
	}
	return w
}

// readDecisionFunc reads what fn calls, the record.Notice literals it writes, the review
// sentences its string constants hold and its loops over kept decisions that write them as text.
func readDecisionFunc(pkg *checkedPackage, fn *ast.FuncDecl) *decisionFunc {
	info := pkg.info
	f := &decisionFunc{pkg: pkg.name, name: info.Defs[fn.Name].(*types.Func).FullName(), at: pkg.pos(fn)}
	// called is the function ident names, by FullName, or "" when it names none.
	called := func(ident *ast.Ident) string {
		if fun, ok := info.Uses[ident].(*types.Func); ok {
			return fun.Origin().FullName()
		}
		return ""
	}
	callsIn := func(node ast.Node) []string {
		var calls []string
		ast.Inspect(node, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				if name := called(ident); name != "" && !slices.Contains(calls, name) {
					calls = append(calls, name)
				}
			}
			return true
		})
		return calls
	}
	f.calls = callsIn(fn.Body)
	f.asks = slices.Contains(f.calls, decisionRule)
	f.picks = slices.Contains(f.calls, senderModule+"record.SpecDecision")
	assigned := localAssignments(info, fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case ast.Expr:
			if value := info.Types[node].Value; value != nil && value.Kind() == constant.String {
				text := constant.StringVal(value)
				for _, sentence := range decisionReviewSentences {
					if strings.Contains(text, sentence.phrase) && !slices.Contains(f.phrases, sentence.phrase) {
						f.phrases = append(f.phrases, sentence.phrase)
					}
				}
				return false
			}
			if lit, ok := node.(*ast.CompositeLit); ok && isRecordType(info.Types[lit].Type, "Notice") {
				readNoticeKind(pkg, f, lit, assigned)
			}
		case *ast.RangeStmt:
			if loop, ok := readKeptLoop(pkg, node, callsIn); ok {
				f.keptLoops = append(f.keptLoops, loop)
			}
		}
		return true
	})
	return f
}

// readNoticeKind adds to f the spec-decision kind a record.Notice literal writes, or the literal's
// position to f.unread when its kind is no constant the walk can read: a constant, or a local
// every assignment of which is one.
func readNoticeKind(pkg *checkedPackage, f *decisionFunc, lit *ast.CompositeLit, assigned map[types.Object][]ast.Expr) {
	for _, elt := range lit.Elts {
		keyed, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			f.unread = append(f.unread, pkg.pos(lit))
			return
		}
		if key, ok := keyed.Key.(*ast.Ident); !ok || key.Name != "Kind" {
			continue
		}
		values, ok := constantStrings(pkg.info, keyed.Value, assigned)
		if !ok {
			f.unread = append(f.unread, pkg.pos(lit))
			return
		}
		for _, value := range values {
			if _, spec := record.SpecDecision(record.NoticeKind(value)); spec && !slices.Contains(f.kinds, value) {
				f.kinds = append(f.kinds, value)
			}
		}
	}
}

// constantStrings is every string expr can hold: its constant value, or the constant value of
// each assignment of the local variable it names.
func constantStrings(info *types.Info, expr ast.Expr, assigned map[types.Object][]ast.Expr) ([]string, bool) {
	if value := info.Types[expr].Value; value != nil && value.Kind() == constant.String {
		return []string{constant.StringVal(value)}, true
	}
	ident, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return nil, false
	}
	values := assigned[info.Uses[ident]]
	if len(values) == 0 {
		return nil, false
	}
	var all []string
	for _, value := range values {
		if value == nil {
			return nil, false
		}
		held := info.Types[value].Value
		if held == nil || held.Kind() != constant.String {
			return nil, false
		}
		all = append(all, constant.StringVal(held))
	}
	return all, true
}

// localAssignments is every value body assigns to each variable it declares or assigns, function
// literals included; a value no single expression gives (a tuple's) is nil.
func localAssignments(info *types.Info, body *ast.BlockStmt) map[types.Object][]ast.Expr {
	assigned := map[types.Object][]ast.Expr{}
	object := func(ident *ast.Ident) types.Object {
		if defined := info.Defs[ident]; defined != nil {
			return defined
		}
		return info.Uses[ident]
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || ident.Name == "_" {
					continue
				}
				var value ast.Expr
				if len(node.Lhs) == len(node.Rhs) {
					value = node.Rhs[i]
				}
				assigned[object(ident)] = append(assigned[object(ident)], value)
			}
		case *ast.ValueSpec:
			for i, ident := range node.Names {
				var value ast.Expr
				if i < len(node.Values) {
					value = node.Values[i]
				}
				assigned[object(ident)] = append(assigned[object(ident)], value)
			}
		}
		return true
	})
	return assigned
}

// readKeptLoop reads loop when it ranges over a list of kept decisions and writes a decision's
// kind (the field Decision of a kept decision) into text.
func readKeptLoop(pkg *checkedPackage, loop *ast.RangeStmt, callsIn func(ast.Node) []string) (keptLoop, bool) {
	info := pkg.info
	slice, ok := info.TypeOf(loop.X).Underlying().(*types.Slice)
	if !ok || !isKeptDecision(slice.Elem()) {
		return keptLoop{}, false
	}
	namesKind := func(expr ast.Expr) bool {
		found := false
		ast.Inspect(expr, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Decision" && isKeptDecision(info.TypeOf(sel.X)) {
				found = true
			}
			return !found
		})
		return found
	}
	writes := false
	ast.Inspect(loop.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && writesText(info, call) && slices.ContainsFunc(call.Args, namesKind) {
			writes = true
		}
		return !writes
	})
	return keptLoop{at: pkg.pos(loop), calls: callsIn(loop.Body)}, writes
}

// writesText is whether call writes its arguments into text: a print of fmt (an error's text,
// fmt.Errorf, reaches no person), a WriteString, or a conversion to string.
func writesText(info *types.Info, call *ast.CallExpr) bool {
	if typ := info.Types[call.Fun]; typ.IsType() {
		basic, ok := typ.Type.Underlying().(*types.Basic)
		return ok && basic.Info()&types.IsString != 0
	}
	var ident *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	default:
		return false
	}
	fun, ok := info.Uses[ident].(*types.Func)
	if !ok {
		return false
	}
	if fun.Name() == "WriteString" {
		return true
	}
	return fun.Pkg() != nil && fun.Pkg().Path() == "fmt" &&
		slices.ContainsFunc([]string{"Sprint", "Fprint", "Append"}, func(prefix string) bool { return strings.HasPrefix(fun.Name(), prefix) })
}

// isRecordType is whether typ is the record package's type name.
func isRecordType(typ types.Type, name string) bool {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == senderModule+"record" && named.Obj().Name() == name
}

// isKeptDecision is whether typ is one of the record types that hold one kept decision.
func isKeptDecision(typ types.Type) bool {
	return slices.ContainsFunc(keptDecisionTypes, func(name string) bool { return isRecordType(typ, name) })
}

// compiledDependencies is where the compiler left the types of everything this package imports,
// as supervise's source tests read them: the build that runs this test has written them into its
// cache, and the go command reports the file of each, so the walk reads those rather than
// type-checking the dependency tree from its source. This package imports the other two sender
// packages and record, so the one listing serves the four packages the walk checks.
func compiledDependencies(t *testing.T) func(string) (io.ReadCloser, error) {
	t.Helper()
	listed, err := exec.Command("go", "list", "-deps", "-export", "-json=ImportPath,Export", ".").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("ask the go command where this package's dependencies were compiled: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("ask the go command where this package's dependencies were compiled: %v", err)
	}
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(listed))
	for decoder.More() {
		var dependency struct{ ImportPath, Export string }
		if err := decoder.Decode(&dependency); err != nil {
			t.Fatalf("read the go command's answer: %v", err)
		}
		if dependency.Export != "" {
			exports[dependency.ImportPath] = dependency.Export
		}
	}
	return func(path string) (io.ReadCloser, error) {
		export, found := exports[path]
		if !found {
			return nil, fmt.Errorf("the go command reported no compiled types for %s", path)
		}
		return os.Open(export)
	}
}
