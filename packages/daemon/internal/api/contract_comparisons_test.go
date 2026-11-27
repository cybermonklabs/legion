package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// contractComparisonRow is one row of the Design's table of what each comparison of a contract
// number reads (dispatch://INFRA-1569/spec, "What each comparison reads"), under that row's own
// name. compares are the comparisons of the code the row names. declares are the declarations
// it names where its code is no comparison of its own: a number or a table that another row's
// comparison reads, a check that the walk cannot read as Go (a script the daemon renders), or a
// call of a check another row holds. A row holds at least one of the two.
type contractComparisonRow struct {
	row      string
	compares []comparisonSite
	declares []declarationSite
}

// comparisonSite is one comparison a row names: file is its path under packages/daemon, function
// the function it is in (`T.name` for a method of T), and forms the comparison as the walk writes
// it (contractComparison.expr), each a way the one comparison may read. A `*` in a form stands for
// a run of letters, digits, `_` and `.`: the stand-ins' `restore < *.contract` is the comparison
// with a row of the stand-in table. A site names every comparison of its function that one of its
// forms matches, and names at least one.
type comparisonSite struct {
	file, function string
	forms          []string
}

// comparedIn is the site of the comparison of function in file that reads as one of forms.
func comparedIn(file, function string, forms ...string) comparisonSite {
	return comparisonSite{file: file, function: function, forms: forms}
}

// declarationSite is one declaration a row names: file is its path under packages/daemon, and
// name the name it declares at package scope (`T.name` for a method of T).
type declarationSite struct{ file, name string }

// contractComparisonRows is the Design's table, one row for each of its rows, in its order. Each
// comparison the walk lists (walkContractComparisons) is named by a row here, and each site of a
// row names a comparison the walk lists or a declaration the code holds. A comparison that
// arrives with no row fails TestEveryContractComparisonHasARow, so the change that adds it says
// which row of the Design's table it belongs to, or adds one there and here.
//
// The rows name DaemonAPIVersion as the call DaemonAPIVersion(), as the code calls it once the
// registry counts its entries (contractExpr writes the constant the same way), and the stand-ins
// both as fixAttemptsContract and as a row of the stand-in table, so each row holds before the
// registry and after it.
var contractComparisonRows = []contractComparisonRow{
	{row: "The pod update op", declares: []declarationSite{
		{"internal/supervise/plugin.go", "PodUpdateContract"},
		{"internal/api/shaping.go", "stateNarrowings"},
	}},
	{row: "Memory and wakes-waiting bounds", declares: []declarationSite{
		{"internal/api/shaping.go", "memoryContract"},
		{"internal/api/shaping.go", "wakesWaitingContract"},
	}},
	{row: "The owner's plan", declares: []declarationSite{
		{"internal/api/plan.go", "OwnerPlanContract"},
	}},
	// The release records' strip of the claims' tokens below the stranded token's entry is the
	// same answer's own row of shapeAnswer as the pause's strip of a resume's `pause`.
	{row: "The pause and plan refusals", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "shapeAnswer", "strip < pauseContract"),
		comparedIn("internal/api/shaping.go", "shapeAnswer", "strip < OwnerPlanContract"),
		comparedIn("internal/api/shaping.go", "shapeAnswer", "strip < entryNumber(strandedTokenContract)"),
	}},
	{row: "A caller's recorded contract", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "recordedContractAtOrBelow", "n >= DaemonAPIVersion()"),
		comparedIn("internal/api/shaping.go", "restoreContract", "n < ServingFloor"),
		comparedIn("internal/api/shaping.go", "stripContract", "n < ServingFloor"),
		comparedIn("internal/api/shaping.go", "server.servedContracts", "lowest < ServingFloor"),
	}},
	{row: "Bands", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "stateFor", "strip < narrowing.contract"),
		comparedIn("internal/api/shaping.go", "shapeIssue", "strip < narrowing.contract"),
	}},
	{row: "Stand-ins", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "stateFor", "restore < fixAttemptsContract", "restore < *.contract"),
		comparedIn("internal/api/shaping.go", "shapeIssue", "restore < fixAttemptsContract", "restore < *.contract"),
	}},
	{row: "Value strips", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "fieldArrivals.heldBack", "restore < arrival"),
	}},
	{row: "The copy of the issue map", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "stateFor", "restore < DaemonAPIVersion()"),
		comparedIn("internal/api/shaping.go", "stateFor", "strip < DaemonAPIVersion()"),
	}},
	{row: "A pod's claims that state their contract", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "podContracts", "c.PluginContract >= shapedAnswersContract"),
		comparedIn("internal/api/shaping.go", "podContracts", "c.PluginContract < lowest"),
		comparedIn("internal/api/shaping.go", "podContracts", "c.PluginContract > highest"),
	}},
	{row: "A caller above the daemon's own contract", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "server.servedContracts", "highest > DaemonAPIVersion()"),
	}},
	{row: "A fresh session's stop read as a failure", compares: []comparisonSite{
		comparedIn("internal/api/freshsession.go", "server.readsTheStopAsAFailure", "at.restore < processReadContract"),
	}},
	{row: "A plan rating is required", compares: []comparisonSite{
		comparedIn("internal/api/handoff.go", "server.answerCompletion", "c.PluginContract >= PlanRatingContract"),
	}},
	// The daemon that knows no contract of its own marks no edge (Deps.PluginContract 0), which
	// is the roll edge's own first term.
	{row: "The roll edge", compares: []comparisonSite{
		comparedIn("internal/supervise/machine.go", "Machine.markRollEdge", "m.deps.PluginContract <= 0"),
		comparedIn("internal/supervise/machine.go", "Machine.markRollEdge", "ev.PluginContract >= m.deps.PluginContract"),
	}},
	{row: "Settings and removed routes", declares: []declarationSite{
		{"internal/api/answers.go", "typedSettings"},
		{"internal/api/requests.go", "RemovedRoutes"},
	}},
	{row: "The added-key threshold", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "lowestPluginContract", "c.PluginContract < lowest"),
	}},
	{row: "No contract", compares: []comparisonSite{
		comparedIn("internal/api/shaping.go", "statedContract", "contract <= 0"),
		comparedIn("internal/api/shaping.go", "recordsContract", "c.PluginContract != 0"),
		comparedIn("internal/runtime/sandbox/probe.go", "Runtime.ProbeImage", "p.Contract < 1"),
	}},
	// readDeclaredContract is the one equality of a manifest with the gate's contract, which the
	// pane's and the pod's lanes run on pi-legion; the claude-envoy root's row below is its call
	// on that root's manifest. The default of `legion probe-image --daemon-api-version` is
	// DaemonAPIVersion itself, which newProbeImageCommand writes.
	{row: "The boot gate and the image probe", compares: []comparisonSite{
		comparedIn("internal/daemon/bootgate.go", "readDeclaredContract", "number == float64(contract)"),
		comparedIn("internal/runtime/sandbox/probe.go", "Runtime.judge", "confirmed != contract"),
	}, declares: []declarationSite{
		{"cmd/legion/probe_image.go", "newProbeImageCommand"},
	}},
	{row: "The claude-envoy root", declares: []declarationSite{
		{"internal/daemon/bootgate.go", "pluginGate.verifyClaude"},
	}},
	{row: "The kept pod's plugin install", declares: []declarationSite{
		{"internal/runtime/sandbox/plugins.go", "pluginCheckScript"},
	}},
	// The 409 STALE_PLUGIN_CONTRACT is written where StaleRefusesCalls says the claim is stale
	// (internal/api's staleClaim), and compares nothing of its own.
	{row: "Stale claims", compares: []comparisonSite{
		comparedIn("internal/supervise/plugin.go", "StaleRefusesCalls", "c.PluginContract == daemonContract"),
		comparedIn("internal/supervise/plugin.go", "Machine.refreshPlugin", "m.claim.PluginContract == m.deps.PluginContract"),
		comparedIn("internal/supervise/plugin.go", "Machine.refreshOverturned", "m.claim.PluginContract == m.deps.PluginContract"),
		comparedIn("internal/supervise/plugin.go", "Machine.staleRegistration", "got == m.deps.PluginContract"),
		comparedIn("internal/supervise/plugin.go", "Machine.staleRegistration", "m.claim.PluginRelaunchFor == m.deps.PluginContract"),
		comparedIn("internal/daemon/stale_plugin.go", "staleForTheLane", "c.PluginContract == api.DaemonAPIVersion()"),
		comparedIn("internal/daemon/specs.go", "specs.stalePluginInstall", "c.PluginContract == api.DaemonAPIVersion()"),
	}},
	{row: "A current registration", compares: []comparisonSite{
		comparedIn("internal/supervise/table.go", "register", "r.PluginContract == m.deps.PluginContract"),
		comparedIn("internal/supervise/table.go", "reregister", "r.PluginContract == m.deps.PluginContract"),
	}},
	// The secret's refusal says whether the request named a contract at all, which is its second
	// comparison.
	{row: "The controller", compares: []comparisonSite{
		comparedIn("internal/api/controller.go", "server.controllerSecret", "req.PluginContract != DaemonAPIVersion()"),
		comparedIn("internal/api/controller.go", "server.controllerSecret", "req.PluginContract != 0"),
		comparedIn("internal/api/controller.go", "server.registerController", "req.PluginContract != DaemonAPIVersion()"),
		comparedIn("internal/api/controller.go", "server.refuseIfStaleController", "stored.PluginContract == DaemonAPIVersion()"),
	}},
	// A claim of a role the deployment defines that registers from a plugin below
	// deploymentRolesContract's entry is refused, since the answer's role is read with an
	// enumeration of Legion's six there (TestEachRoleRowIsTheSixBelowTheEntryAndOpenFromIt).
	{row: "A deployment role's registration", compares: []comparisonSite{
		comparedIn("internal/api/claims.go", "server.register",
			"req.PluginContract < entryNumber(deploymentRolesContract)"),
	}},
	{row: "The idle rule", compares: []comparisonSite{
		comparedIn("internal/workflow/idle.go", "IdleRelease", "c.PluginContract != contract"),
	}},
}

// Every comparison of a contract number in the daemon's code has a row of the Design's table
// (contractComparisonRows), so a comparison that changes what a contract number means is read
// against that table before it merges. The test walks every Go file of packages/daemon that is
// not a test (walkContractComparisons) and fails on each comparison no row names, by its file,
// line and function, and on each site of a row that names no comparison or declaration the code
// holds, so neither the table nor the code moves alone.
//
// It also refuses a comparison with an entry's id constant (build-contract: the untyped constant
// of a contract_<id>_<slug>.go file, whose value is the 14-digit id), which is a time and no
// contract number: the entry's number is entryNumber of that constant, L plus the entry's rank in
// the build's order.
func TestEveryContractComparisonHasARow(t *testing.T) {
	walk := walkContractComparisons(t, daemonModuleRoot(t))
	if len(walk.comparisons) == 0 {
		t.Fatal("the walk lists no comparison of a contract number, so it read nothing")
	}
	named := make([]bool, len(walk.comparisons))
	for _, row := range contractComparisonRows {
		if len(row.compares) == 0 && len(row.declares) == 0 {
			t.Errorf("row %q names no code: name its comparisons, or the declarations its code is", row.row)
		}
		for _, site := range row.compares {
			patterns := make([]*regexp.Regexp, len(site.forms))
			for i, form := range site.forms {
				patterns[i] = formPattern(form)
			}
			found := false
			for i, c := range walk.comparisons {
				if c.file == site.file && c.function == site.function && slices.ContainsFunc(patterns, func(p *regexp.Regexp) bool { return p.MatchString(c.expr) }) {
					named[i], found = true, true
				}
			}
			if !found {
				t.Errorf("row %q names %s in %s of %s, and the walk lists no such comparison there: correct the site to the code, or take it out of the Design's table and this one if the comparison is gone",
					row.row, strings.Join(site.forms, " or "), site.function, site.file)
			}
		}
		for _, site := range row.declares {
			if !walk.declared[site.file][site.name] {
				t.Errorf("row %q names %s in %s, which declares no such name: correct the row to the code", row.row, site.name, site.file)
			}
		}
	}
	for i, c := range walk.comparisons {
		if !named[i] {
			t.Errorf("%s:%d, in %s: %s compares a contract number, and no row of the table names it: name it in the row of the Design's table (dispatch://INFRA-1569/spec, \"What each comparison reads\") whose code it is, or add a row there and here",
				c.file, c.line, c.function, c.expr)
		}
	}
	for _, c := range walk.entryIDs {
		t.Errorf("%s:%d, in %s: %s compares %s, an entry's id, which is the time the entry was made and no contract number: compare the entry's number, entryNumber(%s)",
			c.file, c.line, c.function, c.expr, c.id, c.id)
	}
}

// contractComparison is one comparison the walk lists: file is the path of its file under
// packages/daemon and line its line there, function the function it is in (`T.name` for a method
// of T, the declared names for one at package scope), and expr the comparison as
// types.ExprString writes it, DaemonAPIVersion written as the call (contractExpr).
type contractComparison struct {
	file     string
	line     int
	function string
	expr     string
}

// entryIDComparison is a comparison one of whose operands is the entry id constant id.
type entryIDComparison struct {
	contractComparison
	id string
}

// contractWalk is what walkContractComparisons read: each comparison of a contract number, each
// comparison with an entry's id constant, and the names each file declares at package scope.
type contractWalk struct {
	comparisons []contractComparison
	entryIDs    []entryIDComparison
	declared    map[string]map[string]bool
}

// comparisonOperators are the six operators of a comparison.
var comparisonOperators = map[token.Token]bool{
	token.LSS: true, token.LEQ: true, token.GTR: true, token.GEQ: true, token.EQL: true, token.NEQ: true,
}

// walkContractComparisons reads every Go file under root that is not a test, as the go command
// reads the module's packages: a directory named testdata, or whose name begins with `.` or `_`,
// holds none. It lists each comparison whose operand is one the table holds (contractOperand) and
// whose other operand is not nil, each comparison with an entry's id constant (entryIDOperand),
// and each file's names at package scope.
func walkContractComparisons(t *testing.T, root string) contractWalk {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if file != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = parsed
		return nil
	})
	if err != nil {
		t.Fatalf("read the Go files under %s: %v", root, err)
	}
	ids := entryIDConstants(files)
	walk := contractWalk{declared: map[string]map[string]bool{}}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		walk.declared[rel] = map[string]bool{}
		for _, decl := range files[rel].Decls {
			for _, scope := range declarationScopes(decl) {
				for _, name := range scope.names {
					walk.declared[rel][name] = true
				}
				function := strings.Join(scope.names, ", ")
				ast.Inspect(scope.node, func(n ast.Node) bool {
					compare, isBinary := n.(*ast.BinaryExpr)
					if !isBinary || !comparisonOperators[compare.Op] || isNil(compare.X) || isNil(compare.Y) {
						return true
					}
					at := contractComparison{file: rel, line: fset.Position(compare.OpPos).Line, function: function, expr: contractExpr(compare)}
					for _, operand := range []ast.Expr{compare.X, compare.Y} {
						if id := entryIDOperand(operand, ids); id != "" {
							walk.entryIDs = append(walk.entryIDs, entryIDComparison{contractComparison: at, id: id})
						}
					}
					if contractOperand(compare.X) || contractOperand(compare.Y) {
						walk.comparisons = append(walk.comparisons, at)
					}
					return true
				})
			}
		}
	}
	return walk
}

// declarationScope is one part of a declaration at package scope and the names it declares.
type declarationScope struct {
	node  ast.Node
	names []string
}

// declarationScopes is each part of decl that declares names at package scope, in the order of
// the source: a function with its name (`T.name` for a method of T), and each spec of a const,
// var or type declaration with its own names.
func declarationScopes(decl ast.Decl) []declarationScope {
	switch decl := decl.(type) {
	case *ast.FuncDecl:
		name := decl.Name.Name
		if decl.Recv != nil && len(decl.Recv.List) == 1 {
			name = receiverName(decl.Recv.List[0].Type) + "." + name
		}
		return []declarationScope{{node: decl, names: []string{name}}}
	case *ast.GenDecl:
		var scopes []declarationScope
		for _, spec := range decl.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				scope := declarationScope{node: spec}
				for _, name := range spec.Names {
					scope.names = append(scope.names, name.Name)
				}
				scopes = append(scopes, scope)
			case *ast.TypeSpec:
				scopes = append(scopes, declarationScope{node: spec, names: []string{spec.Name.Name}})
			}
		}
		return scopes
	}
	return nil
}

// receiverName is the name of a method's receiver type: T of `T`, `*T`, `T[K]` and `*T[K]`.
func receiverName(receiver ast.Expr) string {
	for {
		switch e := receiver.(type) {
		case *ast.StarExpr:
			receiver = e.X
		case *ast.ParenExpr:
			receiver = e.X
		case *ast.IndexExpr:
			receiver = e.X
		case *ast.IndexListExpr:
			receiver = e.X
		case *ast.Ident:
			return e.Name
		default:
			return types.ExprString(receiver)
		}
	}
}

// isNil is whether e is nil, a comparison with which says whether a value is there and nothing of
// its number.
func isNil(e ast.Expr) bool {
	ident, isIdent := ast.Unparen(e).(*ast.Ident)
	return isIdent && ident.Name == "nil"
}

// apiVersionConstant is DaemonAPIVersion as the constant, which contractExpr writes as the call.
var apiVersionConstant = regexp.MustCompile(`\bDaemonAPIVersion\b(\(\))?`)

// contractExpr is compare as types.ExprString writes it, with DaemonAPIVersion written as the
// call DaemonAPIVersion() where the constant stands: the build contract makes it the function of
// the registry, the same number either way, and a row names it in the one form.
func contractExpr(compare *ast.BinaryExpr) string {
	return apiVersionConstant.ReplaceAllString(types.ExprString(compare), "DaemonAPIVersion()")
}

// contractOperand is whether e is an operand whose every comparison the table holds:
// DaemonAPIVersion, ServingFloor, a name that ends in Contract or contract, or the restore or
// strip of a placement, each as a name or as the last name of a selector; a call of such a name,
// as DaemonAPIVersion() is; and such an operand in parentheses, dereferenced, or as the one
// argument of a conversion or a call (float64(contract), entryNumber(probeContract)).
func contractOperand(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return contractOperand(e.X)
	case *ast.StarExpr:
		return contractOperand(e.X)
	case *ast.Ident:
		return contractName(e.Name)
	case *ast.SelectorExpr:
		return contractName(e.Sel.Name)
	case *ast.CallExpr:
		return contractOperand(e.Fun) || len(e.Args) == 1 && contractOperand(e.Args[0])
	}
	return false
}

// contractName is whether name is one of contractOperand's names.
func contractName(name string) bool {
	switch name {
	case "DaemonAPIVersion", "ServingFloor", "restore", "strip":
		return true
	}
	return strings.HasSuffix(name, "Contract") || strings.HasSuffix(name, "contract")
}

// entryFileName is the name of an entry's file in internal/api (build-contract: the 14-digit id
// and the slug), and entryIDLiteral the value its id constant is written as.
var (
	entryFileName  = regexp.MustCompile(`^contract_[0-9]{14}_[a-z0-9]+(-[a-z0-9]+)*\.go$`)
	entryIDLiteral = regexp.MustCompile(`^[0-9]{14}$`)
)

// entryIDConstants is the name of each entry's id constant: each untyped constant that an entry's
// file in internal/api declares with a 14-digit literal as its value.
func entryIDConstants(files map[string]*ast.File) map[string]bool {
	ids := map[string]bool{}
	for rel, file := range files {
		if dir, name := path.Split(rel); dir != "internal/api/" || !entryFileName.MatchString(name) {
			continue
		}
		for _, decl := range file.Decls {
			gen, isGen := decl.(*ast.GenDecl)
			if !isGen || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue || value.Type != nil {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						break
					}
					if literal, isLiteral := value.Values[i].(*ast.BasicLit); isLiteral && literal.Kind == token.INT && entryIDLiteral.MatchString(literal.Value) {
						ids[name.Name] = true
					}
				}
			}
		}
	}
	return ids
}

// entryIDOperand is the entry id constant e is, alone, in parentheses or as the one argument of a
// conversion, and "" where e is none or is entryNumber's argument, which resolves the id to the
// entry's number.
func entryIDOperand(e ast.Expr, ids map[string]bool) string {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return entryIDOperand(e.X, ids)
	case *ast.Ident:
		if ids[e.Name] {
			return e.Name
		}
	case *ast.CallExpr:
		if fun, isIdent := ast.Unparen(e.Fun).(*ast.Ident); len(e.Args) == 1 && !(isIdent && fun.Name == "entryNumber") {
			return entryIDOperand(e.Args[0], ids)
		}
	}
	return ""
}

// formPattern is form as an expression that matches the comparisons it names: the form itself,
// each `*` in it standing for a run of letters, digits, `_` and `.`.
func formPattern(form string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(form), `\*`, `[A-Za-z0-9_.]*`) + "$")
}

// daemonModuleRoot is packages/daemon, the directory of the go.mod nearest above the test's own.
func daemonModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
