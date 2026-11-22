package supervise

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	gotoken "go/token"
	"go/types"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// The policy that decides whether a claim's failure charges a budget (Charges) has a chokepoint:
// the charge functions (chargeFunctions, below) each ask it, so no site that reaches one can
// charge what the policy waives. But the Go type system cannot require that a failing path call
// one of them at all, and a busy path that decides the busy answer itself, from the agent's words,
// never reaches a charge function. Nor can a type keep a site from reading those words: they reach
// the daemon as an error and as strings, which it must hold to log a failure and to keep a
// delivery's last send error, and the text of either is open to any function that holds it. Of
// the Claude Code bridge's busy refusal, whose words the bridge keeps for a daemon rolled back to
// the release before this one, the compiler holds one part: the words are a constant the bridge
// does not export (claudebridge's busyWords), so no other package can name them. It cannot keep a
// package from writing them out, nor the bridge from using its constant for anything but that
// refusal. So this test reads the source and holds the five things the compiler cannot: every
// member of the set (runtime.Outcome, declared in internal/runtime) has a row in the policy; no
// site matches a refusal's words — not in this package, nor in internal/runtime, which reads them
// from a send's error (RefusalReason), internal/stream, which mints the outcome where the frame is
// read, or internal/claudebridge, which writes the busy refusal; no site holds the busy refusal's
// words but the refusal itself — none in this package, the stream or the runtime, and none in the
// bridge but its constant and the refusal it writes; every charge names a declared member; and no
// function but a charge function writes a budget counter except to reset it to zero
// (budgetResets names each that may). Delete a check here when the compiler comes to hold it.
//
// The negative control of the check c-the-outcome-is-not-a-string, in dispatch://INFRA-3136's
// verification.md, names each shape of a match this test claims to find, and
// TestOutcomeSourceFindsEachPlantedShape plants each one in a copy of one of these four packages
// and holds the analysis here to finding it. The test follows the text from the sources named in
// the negative control (refusalWords):
//
//   - within one function, and through a variable that ranges over a list or a map of the texts;
//   - into the parameters of the package's own declared functions (assignments);
//   - into a field of a local variable, within the function that writes the field
//     (fieldAssignments);
//   - through a pointer to it;
//   - through a conversion of it to a type whose underlying type is string, such as
//     runtime.Outcome(refusal.Error), and on from the conversion as from the text itself;
//   - into a variable of the package, a package-level string, written in one function and read
//     in another.
//
// It does not follow the text:
//
//   - into a field written in one method and read in another;
//   - out of a helper that returns it;
//   - into a function literal's parameter;
//   - through a method of a named string type;
//   - into a variable of an interface type other than error, or a conversion to one
//     (any(words) == any("..."));
//   - through a copy of a struct (q := note; q.words);
//   - into a comparison of two struct values that hold it (T{a: words} == T{a: "..."});
//   - into a field of a variable of the package;
//   - out of a send's error through fmt.Sprint(err), fmt.Errorf("%w", err).Error() or a channel
//     the error is sent on;
//   - into text built by a strings.Builder, read by a bufio.Scanner or copied into a []byte by
//     copy;
//   - into a comparison of the text's bytes or runes (r[0] == 'A', or a range over the text's
//     runes).
//
// Following all of them is an analysis of the whole program, which a test cannot do easily, so
// the test claims what it follows, and the reviewer reads the rest of each change to these four
// packages for a match of an agent's words.
//
// It type-checks each package's non-test source against the compiler's export data for its
// dependencies (compiledDependencies, as TestProcessEndsListsEveryEndDeclared does), so it reads
// what the compiler resolved — which constant, which field, which function — rather than how a
// line spells it.
func TestOutcomeSourceHoldsThePolicyAtItsSites(t *testing.T) {
	sources := checkOutcomeSources(t)
	src, set := sources[supervisePath], sources[runtimePath]

	t.Run("every declared outcome has a row in Charges", func(t *testing.T) {
		declared := set.declaredOutcomes(t)
		rows := map[string]bool{}
		policy, _ := src.pkg.Scope().Lookup("Charges").(*types.Func)
		charges := src.funcs()[policy]
		if charges == nil {
			t.Fatal("the package declares no function Charges, which this test reads as the policy")
		}
		switches, defaults := 0, 0
		ast.Inspect(charges.Body, func(node ast.Node) bool {
			sw, ok := node.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			switches++
			for _, stmt := range sw.Body.List {
				clause := stmt.(*ast.CaseClause)
				if clause.List == nil {
					defaults++
				}
				for _, expr := range clause.List {
					value := src.info.Types[expr].Value
					if value == nil || !isNamed(src.info.Types[expr].Type, runtimePath, "Outcome") {
						t.Errorf("Charges has a row %s that is no constant Outcome", src.pos(expr))
						continue
					}
					rows[value.ExactString()] = true
				}
			}
			return false
		})
		if switches != 1 {
			t.Fatalf("Charges (%s) holds %d switch statements, want the one switch over the set", src.pos(charges), switches)
		}
		// The zero value's row is the default, which is also the row of every value the set does
		// not hold; every other member has a case of its own, so a member added with none fails
		// here by name rather than falling into the default unseen.
		zero := constant.MakeString("").ExactString()
		if defaults != 1 {
			t.Errorf("Charges has %d default clauses, want one: the default is the row of OutcomeUnrecognised and of every value the set does not hold", defaults)
		}
		if rows[zero] {
			t.Errorf("Charges has a case for OutcomeUnrecognised beside its default, which is that row: delete the case")
		}
		for _, member := range declared {
			if member.value != zero && !rows[member.value] {
				t.Errorf("the outcome %s (%s, %s) has no row in Charges: decide whether it charges", member.name, member.value, member.pos)
			}
		}
	})

	t.Run("no site matches a refusal's words", func(t *testing.T) {
		for _, path := range outcomePaths {
			pkg := sources[path]
			for _, match := range pkg.refusalMatches(t) {
				t.Errorf("%s, in %s, %s matches a refusal's words: the outcome is minted from the frame's code where the frame is read (stream.PromptRefusalOutcome), never from its words",
					pkg.pos(match.at), match.in, match.what)
			}
		}
	})

	t.Run("no site holds the Claude Code bridge's busy words but its busy refusal", func(t *testing.T) {
		words := constant.StringVal(sources[claudebridgePath].bridgeBusyWords(t).Val())
		for _, path := range outcomePaths {
			pkg := sources[path]
			for _, held := range pkg.busyHeld(t, words) {
				t.Errorf("%s, in %s, holds the Claude Code bridge's busy words (claudebridge's busyWords) or a piece of them: only the bridge's busy refusal carries them, for a daemon rolled back to the release before this one, and this one reads a busy agent from the refusal's code (shimwire.CodeSessionBusy), never from its words",
					pkg.pos(held.at), held.in)
			}
		}
	})

	t.Run("every charge names a declared outcome", func(t *testing.T) {
		members := map[string]bool{}
		for _, member := range set.declaredOutcomes(t) {
			members[member.value] = true
		}
		judge := &outcomeJudge{src: src, members: members, judged: map[types.Object]string{}}
		calls := map[string]int{}
		for _, file := range src.files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := src.callee(call)
				if fn == nil || fn.Pkg() != src.pkg || !slices.Contains(chargeFunctions, fn.Name()) {
					return true
				}
				calls[fn.Name()]++
				for i, arg := range call.Args {
					if !isNamed(src.info.Types[arg].Type, runtimePath, "Outcome") {
						continue
					}
					if why := judge.expr(arg); why != "" {
						t.Errorf("%s calls %s with argument %d, which is no outcome the set declares: %s", src.pos(call), fn.Name(), i, why)
					}
				}
				return true
			})
		}
		for _, name := range chargeFunctions {
			if calls[name] == 0 {
				t.Errorf("no site calls %s: this test reads the package's charge functions by name, and that one is gone", name)
			}
		}
	})

	t.Run("no function but a charge function or a reset writes a budget counter", func(t *testing.T) {
		counters := src.budgetCounters(t)
		wrote := map[string]bool{}
		for _, file := range src.files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				for _, write := range src.budgetWrites(fn.Body, counters) {
					wrote[fn.Name.Name] = true
					where := fmt.Sprintf("%s (in %s)", src.pos(write.target), fn.Name.Name)
					switch {
					case slices.Contains(chargeFunctions, fn.Name.Name):
					case budgetResets[fn.Name.Name] != "":
						if !write.reset {
							t.Errorf("%s writes a budget counter other than to zero: %s may only reset the counters, and a charge is the charge functions' alone, which ask the policy (Charges)", where, fn.Name.Name)
						}
					default:
						t.Errorf("%s writes a budget counter: only the charge functions (%s) count against a budget, each asking the policy (Charges), and only the resets listed in budgetResets set one back to zero", where, strings.Join(chargeFunctions, ", "))
					}
				}
			}
		}
		for name := range budgetResets {
			if !wrote[name] {
				t.Errorf("budgetResets lists %s, which writes no budget counter: remove it", name)
			}
		}
		for _, name := range chargeFunctions {
			if !wrote[name] && name != "relaunchAfterFailure" {
				t.Errorf("the charge function %s writes no budget counter: this test reads the charge functions by name, and that one has moved", name)
			}
		}
	})
}

// chargeFunctions are the functions that charge a budget. Each asks Charges itself, and
// relaunchAfterFailure counts through chargeLaunch and writes no counter of its own.
var chargeFunctions = []string{"chargePrompt", "chargeDeath", "relaunchAfterFailure", "chargeLaunch"}

// budgetResets are the functions besides the charge functions that may write a budget counter, and
// each may write one only to zero: a reset is the end of what a budget measures, which is no event
// the policy is asked about.
var budgetResets = map[string]string{
	"confirm":       "a started turn of the task resets the prompt failures and the prompt retirements (Budgets, PromptFailures and PromptRetires)",
	"ready":         "the agent's ready resets the launch failures, since the launch it measured came up (Budgets, LaunchFailures)",
	"turnEnded":     "a turn that ends normally resets the deaths, since the agent got through the work they were charged against (Budgets, Deaths)",
	"retirePending": "the task's retirement resets the deaths charged against it, in the same write (Budgets, Deaths)",
	"retry":         "an operator's retry of a failed claim starts every budget again from nothing (RequestRetry)",
}

// The packages this test reads, by import path.
const (
	internalPath     = "github.com/sjawhar/legion/daemon/internal/"
	supervisePath    = internalPath + "supervise"
	runtimePath      = internalPath + "runtime"
	streamPath       = internalPath + "stream"
	claudebridgePath = internalPath + "claudebridge"
)

// outcomePaths is the packages this test reads, in the order it reads them and reports what it
// finds: this one, the runtime that declares the set, the stream that mints its members and the
// Claude Code bridge.
var outcomePaths = []string{supervisePath, runtimePath, streamPath, claudebridgePath}

// outcomeDirs is the directory of each package this test reads, by import path.
var outcomeDirs = map[string]string{supervisePath: ".", runtimePath: "../runtime", streamPath: "../stream", claudebridgePath: "../claudebridge"}

// budgetCounters is the fields of Budgets, as the type checker holds them.
func (s *goSource) budgetCounters(t *testing.T) map[types.Object]bool {
	t.Helper()
	named, _ := s.pkg.Scope().Lookup("Budgets").(*types.TypeName)
	if named == nil {
		t.Fatal("the package declares no type Budgets, whose fields this test reads as the budget counters")
	}
	fields := named.Type().Underlying().(*types.Struct)
	counters := map[types.Object]bool{}
	for i := range fields.NumFields() {
		counters[fields.Field(i)] = true
	}
	return counters
}

// budgetWrite is one statement writing a budget counter: target is what it writes, and reset
// whether it writes zero by plain assignment.
type budgetWrite struct {
	target ast.Expr
	reset  bool
}

// budgetWrites is every write in body, function literals included, of a budget counter: an
// assignment, an op-assignment or an increment or decrement whose target is a field of Budgets
// through any selector chain or pointer, or a whole value of type Budgets.
func (s *goSource) budgetWrites(body *ast.BlockStmt, counters map[types.Object]bool) []budgetWrite {
	writes := func(target ast.Expr) bool {
		target = ast.Unparen(target)
		if selector, ok := target.(*ast.SelectorExpr); ok {
			if selection := s.info.Selections[selector]; selection != nil && counters[selection.Obj()] {
				return true
			}
		}
		return isNamed(s.info.Types[target].Type, s.pkg.Path(), "Budgets")
	}
	var found []budgetWrite
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.IncDecStmt:
			if writes(node.X) {
				found = append(found, budgetWrite{target: node.X})
			}
		case *ast.AssignStmt:
			for i, target := range node.Lhs {
				if !writes(target) {
					continue
				}
				reset := false
				if node.Tok == gotoken.ASSIGN && len(node.Lhs) == len(node.Rhs) {
					reset = s.isZero(node.Rhs[i])
				}
				found = append(found, budgetWrite{target: target, reset: reset})
			}
		}
		return true
	})
	return found
}

// isZero is whether expr is the constant zero or a composite literal with no elements.
func (s *goSource) isZero(expr ast.Expr) bool {
	expr = ast.Unparen(expr)
	if value := s.info.Types[expr].Value; value != nil {
		return value.ExactString() == "0"
	}
	literal, ok := expr.(*ast.CompositeLit)
	return ok && len(literal.Elts) == 0
}

// goSource is one package's non-test source, type-checked (testtypes.CheckPackage), with its files
// in the order of their names (testtypes.Package.Files), so the test reads them, and reports what
// it finds, in the same order on every run.
type goSource struct {
	fileset *gotoken.FileSet
	files   []*ast.File
	pkg     *types.Package
	info    *types.Info
	// given is what assignments answers, read once on its first call: the refusal taint (spread),
	// the bridge's busy refusal (busyRefusal) and the judge read it, and it walks every file of the
	// package.
	given map[types.Object][]ast.Expr
}

// checkOutcomeSources type-checks the packages this test reads (outcomePaths), by import path.
func checkOutcomeSources(t *testing.T) map[string]*goSource {
	t.Helper()
	fileset := gotoken.NewFileSet()
	imports := outcomeImporter(t, fileset)
	sources := map[string]*goSource{}
	for _, path := range outcomePaths {
		sources[path] = checkOutcomeSource(t, fileset, imports, path, outcomeDirs[path])
	}
	return sources
}

// outcomeImporter opens the compiler's types of what the packages this test reads import
// (compiledDependencies): this package's dependencies hold the runtime's, and the stream's and
// the bridge's are looked up after them.
func outcomeImporter(t *testing.T, fileset *gotoken.FileSet) types.Importer {
	t.Helper()
	return importer.ForCompiler(fileset, "gc", compiledDependencies(t, "../stream", "../claudebridge"))
}

// checkOutcomeSource type-checks the source in dir as the package path (testtypes.CheckPackage):
// one of the packages this test reads, from its own directory or from a copy with a shape
// planted in it (plantShape), which the analysis then reads as it reads that package.
func checkOutcomeSource(t *testing.T, fileset *gotoken.FileSet, imports types.Importer, path, dir string) *goSource {
	t.Helper()
	checked, err := testtypes.CheckPackage(fileset, imports, path, dir)
	if err != nil {
		t.Fatal(err)
	}
	return &goSource{fileset: fileset, files: checked.Files, pkg: checked.Types, info: checked.Info}
}

func (s *goSource) pos(node ast.Node) string { return s.fileset.Position(node.Pos()).String() }

// declName names decl in a failure: the package and the function it declares, a method by its
// receiver's type, or the package's declarations outside any function.
func (s *goSource) declName(decl ast.Decl) string {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "a package-level declaration of " + s.pkg.Name()
	}
	if fn.Recv == nil {
		return s.pkg.Name() + "." + fn.Name.Name
	}
	receiver := types.TypeString(s.info.Defs[fn.Name].(*types.Func).Signature().Recv().Type(), types.RelativeTo(s.pkg))
	if strings.HasPrefix(receiver, "*") {
		receiver = "(" + receiver + ")"
	}
	return s.pkg.Name() + "." + receiver + "." + fn.Name.Name
}

// isNamed is whether typ is the named type the package at path declares as name, read from its
// source or from its export data.
func isNamed(typ types.Type, path, name string) bool {
	named, ok := typ.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && named.Obj().Name() == name
}

type declaredOutcome struct{ name, value, pos string }

// declaredOutcomes is every constant of type Outcome the package's source declares, however it is
// spelled.
func (s *goSource) declaredOutcomes(t *testing.T) []declaredOutcome {
	t.Helper()
	var declared []declaredOutcome
	for ident, object := range s.info.Defs {
		if c, ok := object.(*types.Const); ok && isNamed(c.Type(), runtimePath, "Outcome") {
			declared = append(declared, declaredOutcome{name: ident.Name, value: c.Val().ExactString(), pos: s.pos(ident)})
		}
	}
	if len(declared) == 0 {
		t.Fatalf("found no constant of type Outcome in the source of %s, which this test reads as the set", s.pkg.Path())
	}
	slices.SortFunc(declared, func(a, b declaredOutcome) int { return strings.Compare(a.name, b.name) })
	return declared
}

// callee is the function or method a call resolves to, or nil for a conversion, a builtin or a
// call of a function value.
func (s *goSource) callee(call *ast.CallExpr) *types.Func {
	var ident *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	default:
		return nil
	}
	fn, _ := s.info.Uses[ident].(*types.Func)
	return fn
}

// funcs is every function and method the package declares, by its object, with its declaration.
func (s *goSource) funcs() map[*types.Func]*ast.FuncDecl {
	decls := map[*types.Func]*ast.FuncDecl{}
	for _, file := range s.files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				decls[s.info.Defs[fn.Name].(*types.Func)] = fn
			}
		}
	}
	return decls
}

// params is the objects of a declaration's parameters, in order.
func (s *goSource) params(decl *ast.FuncDecl) []types.Object {
	var objects []types.Object
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			objects = append(objects, nil)
		}
		for _, name := range field.Names {
			objects = append(objects, s.info.Defs[name])
		}
	}
	return objects
}

// busyHeld is every constant in the package's source that holds the Claude Code bridge's busy
// words (busyHolders), in the order of its files, each with the declaration it stands in. The
// bridge may hold them in its busy refusal (busyRefusal); no other package may hold them at all.
func (s *goSource) busyHeld(t *testing.T, words string) []wordMatch {
	t.Helper()
	var allowed map[ast.Expr]bool
	if s.pkg.Path() == claudebridgePath {
		allowed = s.busyRefusal(s.bridgeBusyWords(t))
	}
	var found []wordMatch
	for _, file := range s.files {
		for _, decl := range file.Decls {
			for _, held := range s.busyHolders(decl, words, allowed) {
				found = append(found, wordMatch{at: held, in: s.declName(decl), what: "a constant holding the busy words"})
			}
		}
	}
	return found
}

// bridgeBusyWords is the constant the Claude Code bridge's source declares as its busy words
// (claudebridge's busyWords).
func (s *goSource) bridgeBusyWords(t *testing.T) *types.Const {
	t.Helper()
	busy, _ := s.pkg.Scope().Lookup("busyWords").(*types.Const)
	if busy == nil || busy.Val().Kind() != constant.String || constant.StringVal(busy.Val()) == "" {
		t.Fatal("internal/claudebridge declares no string constant busyWords, whose words this test holds every other site from holding")
	}
	return busy
}

// busyHolders is every constant expression in decl that holds the Claude Code bridge's busy words
// (holdsBusyWords), however it is spelled — the sentence or a piece of it written out, alone, in
// a longer text or in a pattern, a constant of the package's own, the bridge's constant — and
// wherever it stands: an argument, a key or an element of a literal, a list a loop ranges over, a
// variable's value. It reads no flow: nothing in the daemon but the bridge's busy refusal has a
// reason to hold the words, so holding them is the fault, whatever is done with them. Of words
// spelled out in pieces it reports the outermost constant, once. allowed is what may hold them
// (busyRefusal).
func (s *goSource) busyHolders(decl ast.Decl, words string, allowed map[ast.Expr]bool) []ast.Expr {
	var found []ast.Expr
	ast.Inspect(decl, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		if allowed[expr] {
			return false
		}
		if value := s.info.Types[expr].Value; value != nil && value.Kind() == constant.String && holdsBusyWords(constant.StringVal(value), words) {
			found = append(found, expr)
			return false
		}
		return true
	})
	return found
}

// busyRefusal is where the bridge may hold its busy words: the value its declaration gives
// busyWords (assignments), and busyWords as the Error of a shimwire.Response the bridge hands to
// its respond method, which is the busy refusal it writes.
func (s *goSource) busyRefusal(busy *types.Const) map[ast.Expr]bool {
	allowed := map[ast.Expr]bool{}
	for _, value := range s.assignments()[busy] {
		allowed[value] = true
	}
	for _, file := range s.files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn := s.callee(call)
			if fn == nil || fn.Pkg() != s.pkg || fn.Name() != "respond" || fn.Signature().Recv() == nil {
				return true
			}
			for _, arg := range call.Args {
				literal, ok := ast.Unparen(arg).(*ast.CompositeLit)
				if !ok || !isNamed(s.info.Types[literal].Type, internalPath+"shimwire", "Response") {
					continue
				}
				for _, elt := range literal.Elts {
					field, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, isKey := field.Key.(*ast.Ident)
					value, isValue := ast.Unparen(field.Value).(*ast.Ident)
					if isKey && key.Name == "Error" && isValue && s.info.Uses[value] == busy {
						allowed[field.Value] = true
					}
				}
			}
			return true
		})
	}
	return allowed
}

// busyRun is the fewest bytes a constant must share with the busy words, in one run, to hold
// them: "Agent is already", the opening Oh My Pi's own busy sentence shares, is sixteen. A shorter
// run is a phrase other texts share with the words, such as " a turn; the " or " is already ",
// and tells nothing of the refusal.
const busyRun = 16

// bridgeName is the bridge's name as the busy words carry it. Each of the bridge's own messages
// carries it too, so a run of the words that is a piece of it tells nothing of the refusal.
const bridgeName = " the Claude Code bridge "

// holdsBusyWords is whether value shares with words a run of at least busyRun bytes that is no
// piece of the bridge's name (bridgeName): the words whole, a prefix or any other piece of them,
// either alone or inside a longer text.
func holdsBusyWords(value, words string) bool {
	for i := range len(value) - busyRun + 1 {
		for j := range len(words) - busyRun + 1 {
			n := 0
			for i+n < len(value) && j+n < len(words) && value[i+n] == words[j+n] {
				n++
			}
			if n >= busyRun && !strings.Contains(bridgeName, words[j:j+n]) {
				return true
			}
		}
	}
	return false
}

// outcomeJudge says whether an expression of type Outcome is a member the set declares: a
// constant whose value a declared member holds; a field of type Outcome, which the stream filled
// with the member it minted where it read the frame (StreamLateRefusal.Outcome); the member a
// refusal carries, which is the minter's (runtime.RefusalOutcome, read by sendOutcome); a variable
// every expression given it is one of these; or a call of the package's own function every return
// of which is. A conversion to Outcome of anything but a member's constant is none of them.
type outcomeJudge struct {
	src     *goSource
	members map[string]bool
	given   map[types.Object][]ast.Expr
	decls   map[*types.Func]*ast.FuncDecl
	// judged holds each variable or function already judged, and "" for one being judged now, so
	// a cycle — relaunchAfterFailure's retry passes its own parameter back to itself — is judged
	// by the other paths into it.
	judged map[types.Object]string
}

func (j *outcomeJudge) expr(expr ast.Expr) string {
	expr = ast.Unparen(expr)
	if j.given == nil {
		j.given, j.decls = j.src.assignments(), j.src.funcs()
	}
	tv := j.src.info.Types[expr]
	if tv.Value != nil {
		if tv.Value.Kind() != constant.String || !j.members[tv.Value.ExactString()] {
			return fmt.Sprintf("%s is the constant %s, which no member holds", j.src.pos(expr), tv.Value.ExactString())
		}
		return ""
	}
	switch expr := expr.(type) {
	case *ast.Ident:
		object := j.src.info.Uses[expr]
		variable, ok := object.(*types.Var)
		if !ok {
			return fmt.Sprintf("%s names %s, which is no variable", j.src.pos(expr), expr.Name)
		}
		return j.variable(variable, expr)
	case *ast.SelectorExpr:
		selection := j.src.info.Selections[expr]
		if selection == nil {
			return fmt.Sprintf("%s is no field or method of a value", j.src.pos(expr))
		}
		if selection.Kind() == types.FieldVal {
			return ""
		}
		return fmt.Sprintf("%s is a method value, not a member", j.src.pos(expr))
	case *ast.CallExpr:
		if j.src.info.Types[expr.Fun].IsType() {
			return fmt.Sprintf("%s converts a value that is no member's constant to Outcome", j.src.pos(expr))
		}
		fn := j.src.callee(expr)
		if fn == nil {
			return fmt.Sprintf("%s calls a function value whose answer this test cannot read", j.src.pos(expr))
		}
		if fn.Pkg() != nil && fn.Pkg().Path() == runtimePath && fn.Name() == "RefusalOutcome" {
			return ""
		}
		decl, declared := j.decls[fn]
		if !declared {
			return fmt.Sprintf("%s calls %s, which this package does not declare", j.src.pos(expr), fn.Name())
		}
		return j.function(fn, decl)
	}
	return fmt.Sprintf("%s is an expression this test cannot read as a member", j.src.pos(expr))
}

func (j *outcomeJudge) variable(variable *types.Var, at ast.Node) string {
	if why, done := j.judged[variable]; done {
		return why
	}
	j.judged[variable] = ""
	exprs := j.given[variable]
	why := ""
	if len(exprs) == 0 {
		why = fmt.Sprintf("%s reads %s, which nothing in the package gives a value", j.src.pos(at), variable.Name())
	}
	for _, given := range exprs {
		if reason := j.expr(given); reason != "" {
			why = fmt.Sprintf("%s is given %s", variable.Name(), reason)
			break
		}
	}
	j.judged[variable] = why
	return why
}

func (j *outcomeJudge) function(fn *types.Func, decl *ast.FuncDecl) string {
	if why, done := j.judged[fn]; done {
		return why
	}
	j.judged[fn] = ""
	why := ""
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if _, literal := node.(*ast.FuncLit); literal || why != "" {
			return false
		}
		if ret, ok := node.(*ast.ReturnStmt); ok {
			for _, result := range ret.Results {
				if isNamed(j.src.info.Types[result].Type, runtimePath, "Outcome") {
					if reason := j.expr(result); reason != "" {
						why = fmt.Sprintf("%s returns %s", fn.Name(), reason)
					}
				}
			}
		}
		return true
	})
	j.judged[fn] = why
	return why
}
