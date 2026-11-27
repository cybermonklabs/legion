package api

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

// The packages that say what register_plan and stop_task answer of their own: this one, whose
// plan.go holds the two routes and the functions that write their refusals (writeFailure), the
// workflow, whose planGiven and taskStopped decide each fact the routes commit, and intake, whose
// constructors write each refusal the workflow answers with. Of this package only plan.go is
// read: the refusals of the helpers every architect route shares (the grant's redemption, the
// body's decoding, the fact's intake) are no part of this census, since they are no answer of these
// two routes alone.
const (
	planRouteFile     = "plan.go"
	planRouteWorkflow = "../workflow"
	planRouteIntake   = "../intake"
)

// historyEntry opens an entry of version.go's contract history: `// <n>: ` at the start of a
// comment line.
var historyEntry = regexp.MustCompile(`(?m)^// \d+: `)

// Every refusal code register_plan and stop_task answer of their own is named in an entry of the
// daemon API contract history (version.go's numbered entries and each entry's own file), which is where a plugin's author reads what a route
// answers. The codes are read from the type-checked source, not copied: each code a function of
// plan.go reachable from the two routes writes, and each code of a refusal that the workflow's
// planGiven or taskStopped, or a function of the workflow or of intake they reach, answers the fact
// with. A code a change adds to either route fails here until an entry names it, and a code the
// census cannot read fails here naming where it stands (codeCensus.code). The control is the
// newest code taken out of its entry, which this test then names.
func TestEveryCodeThePlanRoutesAnswerIsInTheContractHistory(t *testing.T) {
	census := loadCodeCensus(t)
	route := census.reached(t, census.named(t, "api", "registerPlan", "stopTask"))
	workflow := census.reached(t, census.named(t, "workflow", "planGiven", "taskStopped"))
	if !slices.Contains(workflow, "TREE_LINGERING") || !slices.Contains(route, "VERSION_NOT_IN_DISPATCH") {
		t.Fatalf("the reading of the source found %v in the routes and %v in the workflow, without a code each must hold: the reading itself is broken, and a census of nothing passes nothing",
			route, workflow)
	}
	source, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatal(err)
	}
	first := historyEntry.FindIndex(source)
	if first == nil {
		t.Fatal("version.go holds no `// <n>: ` entry: the contract history could not be read")
	}
	history := string(source[first[0]:])
	// An entry after the numbered ones is a file of its own, whose text is the rest of the history
	// (contracts.go).
	entries, err := filepath.Glob("contract_[0-9]*_*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry, "_test.go") {
			continue
		}
		text, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		history += string(text)
	}
	codes := slices.Compact(slices.Sorted(slices.Values(append(route, workflow...))))
	for _, code := range codes {
		if !regexp.MustCompile(`\b` + code + `\b`).MatchString(history) {
			t.Errorf("register_plan or stop_task can answer %s, and no entry of the contract history (version.go and the entry files) names it", code)
		}
	}
	t.Logf("codes the two routes answer of their own, each held to the history: %v", codes)
}

// codeCensus is the type-checked source of this package, the workflow and intake: each function
// the census may walk into, which is every function of the workflow and of intake and each
// function of this package's plan.go.
type codeCensus struct {
	fset  *token.FileSet
	funcs map[*types.Func]censusFunc
}

// censusFunc is one function the census may walk into: its declaration and the type information of
// its package.
type censusFunc struct {
	decl *ast.FuncDecl
	info *types.Info
}

// loadCodeCensus type-checks this package, the workflow and intake from their non-test source.
func loadCodeCensus(t *testing.T) codeCensus {
	t.Helper()
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  ".",
	}
	pkgs, err := packages.Load(config, ".", planRouteWorkflow, planRouteIntake)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the census could not type-check the source it reads")
	}
	census := codeCensus{funcs: map[*types.Func]censusFunc{}}
	read := map[string]int{}
	for _, pkg := range pkgs {
		census.fset = pkg.Fset
		for _, file := range pkg.Syntax {
			if pkg.Name == "api" && filepath.Base(pkg.Fset.Position(file.Pos()).Filename) != planRouteFile {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				object, ok := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
				if !ok {
					t.Fatalf("%s: %s declares no function the type checker knows", pkg.Fset.Position(fn.Pos()), fn.Name.Name)
				}
				census.funcs[object] = censusFunc{decl: fn, info: pkg.TypesInfo}
				read[pkg.Name]++
			}
		}
	}
	for _, name := range []string{"api", "workflow", "intake"} {
		if read[name] == 0 {
			t.Fatalf("the census read no function of %s: it read %v", name, read)
		}
	}
	return census
}

// named is each function of the package named pkg whose name is one of names, and fails where one
// is missing.
func (c codeCensus) named(t *testing.T, pkg string, names ...string) []*types.Func {
	t.Helper()
	var found []*types.Func
	for _, name := range names {
		at := len(found)
		for object := range c.funcs {
			if object.Pkg().Name() == pkg && object.Name() == name {
				found = append(found, object)
			}
		}
		if len(found) == at {
			t.Fatalf("no function %s in %s", name, pkg)
		}
	}
	return found
}

// reached is every code a function reachable from start writes, walking each static call and each
// call of an interface method into every function of the census that implements it:
//
//   - the Code of a Failure, PlanRefusal or Refusal literal (refusalType);
//   - the argument of each call for a string parameter named code, as writeFailure's, the
//     workflow's refused and intake's staleGeneration take one, whatever the callee.
//
// Each of those expressions is read by code, which fails where it cannot say what code it is.
func (c codeCensus) reached(t *testing.T, start []*types.Func) []string {
	t.Helper()
	seen := map[*types.Func]bool{}
	queue := slices.Clone(start)
	var codes []string
	for len(queue) > 0 {
		object := queue[0]
		queue = queue[1:]
		if seen[object] {
			continue
		}
		seen[object] = true
		fn := c.funcs[object]
		ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CompositeLit:
				if refusalType(fn.info.TypeOf(node)) {
					for _, element := range node.Elts {
						if field, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Code" {
								codes = append(codes, c.code(t, fn, field.Value)...)
							}
						}
					}
				}
			case *ast.CallExpr:
				for _, callee := range c.callees(fn.info, node) {
					params := callee.Signature().Params()
					for i := range min(params.Len(), len(node.Args)) {
						if param := params.At(i); param.Name() == "code" && types.Identical(param.Type(), types.Typ[types.String]) {
							codes = append(codes, c.code(t, fn, node.Args[i])...)
						}
					}
					if _, local := c.funcs[callee]; local {
						queue = append(queue, callee)
					}
				}
			}
			return true
		})
	}
	return codes
}

// callees is the function a call names where it is static (typeutil.StaticCallee), and for a call
// of an interface method the method itself and each function of the census that implements it; a
// call of a function value names nothing.
func (c codeCensus) callees(info *types.Info, call *ast.CallExpr) []*types.Func {
	if callee := typeutil.StaticCallee(info, call); callee != nil {
		return []*types.Func{callee.Origin()}
	}
	selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	selection, ok := info.Selections[selector]
	if !ok || selection.Kind() != types.MethodVal {
		return nil
	}
	iface, ok := selection.Recv().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	method := selection.Obj().(*types.Func)
	callees := []*types.Func{method}
	for object := range c.funcs {
		recv := object.Signature().Recv()
		if recv == nil || object.Name() != method.Name() {
			continue
		}
		if types.Implements(recv.Type(), iface) {
			callees = append(callees, object)
		}
	}
	return callees
}

// code is the refusal code expr names, read from the type checker. A compile-time string constant
// is its value, written as a literal or as a constant of any package. Two other forms carry a code
// another expression of the census produces, and are read as no code of their own: a parameter
// named code of the function expr stands in, whose callers' arguments reached reads at each call;
// and the Code field of another refusal (refusalType), as writeRefusal passes on the one the
// workflow decided. Any other expression fails the census, naming its file and line, since a code
// the census cannot say is no code it can hold to the history, and reading it as no code would
// pass a refusal no entry names.
func (c codeCensus) code(t *testing.T, fn censusFunc, expr ast.Expr) []string {
	t.Helper()
	if value := fn.info.Types[expr].Value; value != nil && value.Kind() == constant.String {
		return []string{constant.StringVal(value)}
	}
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		if variable, ok := fn.info.Uses[expr].(*types.Var); ok && variable.Name() == "code" && parameterOf(fn, variable) {
			return nil
		}
	case *ast.SelectorExpr:
		if selection, ok := fn.info.Selections[expr]; ok && selection.Kind() == types.FieldVal &&
			selection.Obj().Name() == "Code" && refusalType(selection.Recv()) {
			return nil
		}
	}
	t.Errorf("%s: %s, a refusal code in %s, is no compile-time string constant, no parameter named code and no other refusal's Code, so the census cannot say which code it is; make it one of those",
		c.fset.Position(expr.Pos()), types.ExprString(expr), fn.decl.Name.Name)
	return nil
}

// parameterOf is whether variable is a parameter of fn.
func parameterOf(fn censusFunc, variable *types.Var) bool {
	for _, field := range fn.decl.Type.Params.List {
		for _, name := range field.Names {
			if fn.info.Defs[name] == variable {
				return true
			}
		}
	}
	return false
}

// refusalType is whether typ, or what it points to, is one of the answers that carry a refusal's
// code: this package's Failure and PlanRefusal, and intake's Refusal.
func refusalType(typ types.Type) bool {
	if pointer, ok := types.Unalias(typ).(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	switch path, name := named.Obj().Pkg().Path(), named.Obj().Name(); {
	case strings.HasSuffix(path, "/internal/api"):
		return name == "Failure" || name == "PlanRefusal"
	case strings.HasSuffix(path, "/internal/intake"):
		return name == "Refusal"
	}
	return false
}
