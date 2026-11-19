package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lockSite is whose goroutine calls a function of the mention sender that takes its lock
// (mentionSender.mu), as the sender claim model reads it (cmCaller).
type lockSite uint8

const (
	// lockSiteNone is a function the list does not name: the claim model panics if it takes the
	// lock, since a locked section the model does not know of would run outside every step.
	lockSiteNone lockSite = iota
	// lockSiteLane is a function only a lane goroutine calls (serve), each of whose locked
	// sections the model makes a step of its own.
	lockSiteLane
	// lockSiteRefresh is a function only the refresh calls (keepClaims), the same.
	lockSiteRefresh
	// lockSiteModel is a function only the model's own calls make (enqueue, unpark, wait), which
	// take the lock as they come.
	lockSiteModel
	// lockSiteOutsideTheModel is a function the model never calls: the held gauges' read, which no
	// scrape makes in the model. It panics there, as an unnamed one does.
	lockSiteOutsideTheModel
)

// mentionSenderLockSites is every function of the package that takes the mention sender's lock,
// by whose goroutine calls it. The claim model reads a lock's caller off the stack by its
// function's name (cmCaller) and parkGate reads park's the same way (lockCaller, parkLockSite),
// so this list must name those functions as their frames name them:
// TestTheLockSiteListNamesTheSendersLocks holds it to the package's source in every run of this
// package, where the model itself runs only in its own job (envoy-go-claim-model).
var mentionSenderLockSites = map[string]lockSite{
	"next": lockSiteLane, "claimKnown": lockSiteLane, "markTrying": lockSiteLane, "claimedDuring": lockSiteLane,
	"park": lockSiteLane, "reportLate": lockSiteLane,
	"refreshHeldClaims": lockSiteRefresh, "rereadUnsure": lockSiteRefresh,
	"enqueue": lockSiteModel, "unpark": lockSiteModel, "wait": lockSiteModel,
	"heldSamples": lockSiteOutsideTheModel,
}

// parkLockSite is the function whose Lock parkGate holds at its gate: park, a lane's.
const parkLockSite = "park"

// The claim model's list of the sender's lock sites (mentionSenderLockSites) names exactly the
// functions whose frames take the sender's lock as the model reads a frame (cmCaller): each
// function of the package, a method or not, that calls Lock on the sender's mutex
// (mentionSender.mu), reached through any value of the sender's type or through a local variable
// that holds the mutex, and no other name. A lock whose frame the model reads under no function's
// name - one in a function literal or in the body of a range over a function, whose frame is the
// literal's own (funcN, F-rangeN), one in a generic function, whose frame is F[...], or one
// deferred or started on a goroutine, whose frame is the compiler's wrapper - fails it too, and so
// do the mutex handed anywhere the guard cannot follow and a mu selected on a value whose type it
// cannot read. A miss would otherwise make the model panic, and only in the claim model's own job,
// which the full-ci label gates. The controls in the same run read the same source with one fault
// planted each - claimKnown renamed; a lock through a local alias; one in a function literal
// inside a listed method; one in a plain function; the mutex handed to a helper that locks it; a
// lock in a range over a function; a deferred lock; one in a generic function; and a lock on a
// value whose type rests on an import - and the guard must name each.
func TestTheLockSiteListNamesTheSendersLocks(t *testing.T) {
	sources := map[string][]byte{}
	for _, name := range goFilesOfThisPackage(t) {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources[name] = source
	}
	problems := lockSiteProblems(t, sources)
	t.Logf("the list names %d lock sites; problems: %q", len(mentionSenderLockSites), problems)
	if len(problems) != 0 {
		t.Fatalf("the claim model's lock-site list (mentionSenderLockSites) does not match the sender's source:\n%s",
			strings.Join(problems, "\n"))
	}
	if mentionSenderLockSites[parkLockSite] != lockSiteLane {
		t.Fatalf("parkGate holds the Lock of %q, which the list does not name as a lane goroutine's", parkLockSite)
	}

	// Control: the same source with claimKnown renamed claimKnownRenamed, as a rename leaves it.
	const declared = "func (m *mentionSender) claimKnown("
	renamed := plantedLockSources(t, sources, declared, func(source string) string {
		return strings.ReplaceAll(source, declared, "func (m *mentionSender) claimKnownRenamed(")
	})
	planted := lockSiteProblems(t, renamed)
	t.Logf("control, claimKnown renamed: %q", planted)
	if len(planted) != 2 || !strings.Contains(planted[0], `"claimKnown"`) || !strings.Contains(planted[1], `"claimKnownRenamed"`) {
		t.Fatalf("control: with claimKnown renamed the guard reports %q, want the listed name gone and the renamed method unlisted", planted)
	}

	// Controls: a lock site the model cannot name, each planted alone.
	const unpark = "func (m *mentionSender) unpark(lane laneKey) {\n"
	for _, plant := range []struct {
		name string
		// file is the file the plant adds; with no file, the plant's source goes first in the
		// body of unpark, a listed method.
		file, source, want string
	}{
		{name: "a lock through a local alias", file: "planted_alias.go",
			source: "package api\n\nfunc (m *mentionSender) plantedAlias() {\n\tmu := m.mu\n\tmu.Lock()\n\tmu.Unlock()\n}\n",
			want:   `"plantedAlias" takes the sender's lock and the list does not name it`},
		{name: "a lock in a function literal inside a listed method",
			source: "\tfunc() {\n\t\tm.mu.Lock()\n\t\tm.mu.Unlock()\n\t}()\n",
			want:   "a function literal in unpark takes the sender's lock: the claim model reads its frame as func1"},
		{name: "a lock in a plain function", file: "planted_function.go",
			source: "package api\n\nfunc plantedFunction(m *mentionSender) {\n\tm.mu.Lock()\n\tm.mu.Unlock()\n}\n",
			want:   `"plantedFunction" takes the sender's lock and the list does not name it`},
		{name: "the lock handed to a helper that takes it", file: "planted_helper.go",
			source: "package api\n\nimport \"sync\"\n\nfunc (m *mentionSender) plantedHelper() {\n\tlockIt(m.mu)\n}\n\n" +
				"func lockIt(l sync.Locker) {\n\tl.Lock()\n\tl.Unlock()\n}\n",
			want: "the sender's lock is used where the guard cannot follow it"},
		{name: "a lock in the body of a range over a function", file: "planted_range.go",
			source: "package api\n\nfunc (m *mentionSender) plantedRange(each func(func() bool)) {\n\tfor range each {\n\t\tm.mu.Lock()\n\t\tm.mu.Unlock()\n\t}\n}\n",
			want:   "a range over a function in plantedRange takes the sender's lock in its body"},
		{name: "a deferred lock", file: "planted_defer.go",
			source: "package api\n\nfunc (m *mentionSender) plantedDefer() {\n\tdefer m.mu.Lock()\n}\n",
			want:   "a deferred Lock in plantedDefer runs in the compiler's wrapper frame"},
		{name: "a lock in a generic function", file: "planted_generic.go",
			source: "package api\n\nfunc plantedGeneric[T any](m *mentionSender, _ T) {\n\tm.mu.Lock()\n\tm.mu.Unlock()\n}\n",
			want:   "a generic function, plantedGeneric, takes the sender's lock: its frame is named plantedGeneric[...]"},
		{name: "a lock on a value whose type rests on an import", file: "planted_unread.go",
			source: "package api\n\nimport \"slices\"\n\nfunc plantedUnread(senders []*mentionSender) {\n\tfor _, m := range slices.Values(senders) {\n\t\tm.mu.Lock()\n\t}\n}\n",
			want:   "mu is selected on a value whose type the guard cannot read"},
	} {
		var faulty map[string][]byte
		if plant.file != "" {
			faulty = plantedLockSources(t, sources, "", nil)
			faulty[plant.file] = []byte(plant.source)
		} else {
			faulty = plantedLockSources(t, sources, unpark, func(source string) string {
				return strings.Replace(source, unpark, unpark+plant.source, 1)
			})
		}
		found := lockSiteProblems(t, faulty)
		t.Logf("control, %s: %q", plant.name, found)
		if len(found) != 1 || !strings.Contains(found[0], plant.want) {
			t.Fatalf("control: with %s the guard reports %q, want one problem saying %q", plant.name, found, plant.want)
		}
	}
}

// plantedLockSources is a copy of sources with edit made to the one file that holds anchor, which
// it must hold once; with no anchor the copy is unchanged.
func plantedLockSources(t *testing.T, sources map[string][]byte, anchor string, edit func(string) string) map[string][]byte {
	t.Helper()
	planted, found := map[string][]byte{}, 0
	for name, source := range sources {
		planted[name] = source
		if anchor != "" && strings.Contains(string(source), anchor) {
			found += strings.Count(string(source), anchor)
			planted[name] = []byte(edit(string(source)))
		}
	}
	if anchor != "" && found != 1 {
		t.Fatalf("the control finds %q %d times, want once", anchor, found)
	}
	return planted
}

// lockSiteProblems is what is wrong with mentionSenderLockSites against sources, this package's
// non-test files by name (TestTheLockSiteListNamesTheSendersLocks): each name the list holds that
// no function taking the sender's lock has, then, sorted, each function taking it that the list
// does not name, each lock whose frame the model reads under no function's name, and each use of
// the mutex the guard cannot follow or whose type it cannot read.
//
// It type-checks sources alone, each import an empty stub (lockSiteImporter): the package's own
// types are all it needs, since an expression that selects mu selects a field of a type this
// package declares, and the sender's mutex is one object, mentionSender's mu field. An expression
// that selects mu on a value whose type rests on an import is reported, not passed.
func lockSiteProblems(t *testing.T, sources map[string][]byte) []string {
	t.Helper()
	positions := token.NewFileSet()
	files := parseSources(t, positions, sources)
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: lockSiteImporter{}, Error: func(error) {}}
	pkg, _ := config.Check("github.com/sjawhar/envoy/internal/dispatch/api", positions, files, info)
	mutex := senderMutex(t, pkg)

	parents := map[ast.Node]ast.Node{}
	var exprs []ast.Expr
	var literals []*ast.FuncLit
	for _, file := range files {
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) > 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			switch node := node.(type) {
			case *ast.SelectorExpr, *ast.Ident:
				exprs = append(exprs, node.(ast.Expr))
			case *ast.FuncLit:
				literals = append(literals, node)
			}
			return true
		})
	}
	at := func(node ast.Node) string {
		position := positions.Position(node.Pos())
		return position.Filename + ":" + strconv.Itoa(position.Line)
	}
	// local is the local variable expr names, an assignment's or declaration's left side, or nil.
	local := func(expr ast.Expr) *types.Var {
		ident, ok := expr.(*ast.Ident)
		if !ok {
			return nil
		}
		object := info.Defs[ident]
		if object == nil {
			object = info.Uses[ident]
		}
		variable, ok := object.(*types.Var)
		if !ok || variable.IsField() || variable.Parent() == nil || variable.Parent() == pkg.Scope() {
			return nil
		}
		return variable
	}

	// aliases are the local variables that hold the sender's mutex.
	aliases := map[types.Object]bool{}
	isMutex := func(expr ast.Expr) bool {
		switch expr := expr.(type) {
		case *ast.SelectorExpr:
			selection := info.Selections[expr]
			return selection != nil && selection.Obj() == mutex
		case *ast.Ident:
			return aliases[info.Uses[expr]]
		}
		return false
	}
	// use is what expr, the sender's mutex, is used for: the call that locks it, the local
	// variable it is the value of, or, when escapes is false and neither is set, an Unlock or a
	// write of the field or the variable.
	type use struct {
		lock    *ast.CallExpr
		alias   *types.Var
		escapes bool
	}
	usage := func(expr ast.Expr) use {
		parent := parents[expr]
		for paren, ok := parent.(*ast.ParenExpr); ok; paren, ok = parent.(*ast.ParenExpr) {
			expr, parent = paren, parents[paren]
		}
		switch parent := parent.(type) {
		case *ast.SelectorExpr:
			call, called := parents[parent].(*ast.CallExpr)
			switch {
			case parent.X != expr || !called || call.Fun != parent:
			case parent.Sel.Name == "Lock":
				return use{lock: call}
			case parent.Sel.Name == "Unlock":
				return use{}
			}
		case *ast.AssignStmt:
			if slices.Contains(parent.Lhs, expr) {
				return use{}
			}
			if index := slices.Index(parent.Rhs, expr); index >= 0 && len(parent.Lhs) == len(parent.Rhs) {
				if ident, ok := parent.Lhs[index].(*ast.Ident); ok && ident.Name == "_" {
					return use{}
				}
				if variable := local(parent.Lhs[index]); variable != nil {
					return use{alias: variable}
				}
			}
		case *ast.ValueSpec:
			if index := slices.Index(parent.Values, expr); index >= 0 && len(parent.Names) == len(parent.Values) {
				if variable := local(parent.Names[index]); variable != nil {
					return use{alias: variable}
				}
			}
		}
		return use{escapes: true}
	}
	for changed := true; changed; {
		changed = false
		for _, expr := range exprs {
			if alias := usage(expr).alias; isMutex(expr) && alias != nil && !aliases[alias] {
				aliases[alias], changed = true, true
			}
		}
	}

	// enclosing is the function literal or declaration node is in, nil at the package level.
	enclosing := func(node ast.Node) ast.Node {
		for node = parents[node]; node != nil; node = parents[node] {
			switch node.(type) {
			case *ast.FuncLit, *ast.FuncDecl:
				return node
			}
		}
		return nil
	}
	// declared is the name of the declared function node is in.
	declared := func(node ast.Node) string {
		for node = enclosing(node); node != nil; node = enclosing(node) {
			if fn, ok := node.(*ast.FuncDecl); ok {
				return fn.Name.Name
			}
		}
		return "a package-level declaration"
	}
	// literalFrame is the last part of the compiler's name for literal, which the model reads as its
	// frame (cmCaller): funcN for the Nth literal of a declared function, N for the Nth of another
	// literal, counted in source order.
	literalFrame := func(literal *ast.FuncLit) string {
		owner, n := enclosing(literal), 0
		for _, other := range literals {
			if enclosing(other) == owner && other.Pos() <= literal.Pos() {
				n++
			}
		}
		if _, nested := owner.(*ast.FuncLit); nested {
			return strconv.Itoa(n)
		}
		return "func" + strconv.Itoa(n)
	}
	// frame is the declared function whose frame the model reads for a lock at call, or why it
	// reads none.
	frame := func(call *ast.CallExpr) (name, unnamed string) {
		switch parents[call].(type) {
		case *ast.DeferStmt:
			return "", "a deferred Lock in " + declared(call) + " runs in the compiler's wrapper frame (deferwrapN)"
		case *ast.GoStmt:
			return "", "a Lock started on a goroutine in " + declared(call) + " runs in the compiler's wrapper frame (gowrapN)"
		}
		child := ast.Node(call)
		for node := parents[call]; node != nil; child, node = node, parents[node] {
			switch node := node.(type) {
			case *ast.FuncLit:
				return "", "a function literal in " + declared(node) + " takes the sender's lock: the claim model reads its frame as " +
					literalFrame(node) + " (cmCaller), which names no function, and panics; take the lock in a named function"
			case *ast.RangeStmt:
				if child != node.Body {
					continue
				}
				switch over := info.TypeOf(node.X); {
				case over == nil || over.Underlying() == types.Typ[types.Invalid]:
					return "", "a range in " + declared(node) + " over a value whose type the guard cannot read takes the sender's lock " +
						"in its body, which runs as a function of its own when the range is over a function"
				case isSignature(over):
					return "", "a range over a function in " + declared(node) + " takes the sender's lock in its body, " +
						"which runs as a function of its own (" + declared(node) + "-rangeN); take the lock in a named function"
				}
			case *ast.FuncDecl:
				if node.Type.TypeParams.NumFields() > 0 {
					return "", "a generic function, " + node.Name.Name + ", takes the sender's lock: its frame is named " +
						node.Name.Name + "[...], which the claim model reads as ] (cmCaller), names no function, and panics; " +
						"take the lock in a function that is not generic"
				}
				return node.Name.Name, ""
			}
		}
		return "", "a Lock outside every function"
	}

	sites := map[string]string{}
	var rest []string
	for _, expr := range exprs {
		if selector, ok := expr.(*ast.SelectorExpr); ok && selector.Sel.Name == "mu" && info.Selections[selector] == nil {
			rest = append(rest, at(expr)+": mu is selected on a value whose type the guard cannot read")
			continue
		}
		if !isMutex(expr) {
			continue
		}
		use := usage(expr)
		switch {
		case use.escapes:
			rest = append(rest, at(expr)+": the sender's lock is used where the guard cannot follow it")
		case use.lock != nil:
			name, unnamed := frame(use.lock)
			if unnamed != "" {
				rest = append(rest, at(use.lock)+": "+unnamed)
			} else if _, seen := sites[name]; !seen {
				sites[name] = at(use.lock)
			}
		}
	}
	if len(sites) == 0 {
		t.Fatal("the guard found no Lock of the sender's mutex: it read nothing")
	}
	var gone []string
	for name := range mentionSenderLockSites {
		if _, takes := sites[name]; !takes {
			gone = append(gone, `the list names "`+name+`", and no function of that name takes the sender's lock`)
		}
	}
	for name, where := range sites {
		if _, listed := mentionSenderLockSites[name]; !listed {
			rest = append(rest, `"`+name+`" takes the sender's lock and the list does not name it (`+where+`)`)
		}
	}
	slices.Sort(gone)
	slices.Sort(rest)
	return append(gone, rest...)
}

// isSignature is whether a value of type t is a function, which a range runs its body in as a
// function of its own.
func isSignature(t types.Type) bool {
	_, ok := t.Underlying().(*types.Signature)
	return ok
}

// senderMutex is the mu field of pkg's mentionSender, which the guard fails without.
func senderMutex(t *testing.T, pkg *types.Package) *types.Var {
	t.Helper()
	if pkg != nil {
		if sender, ok := pkg.Scope().Lookup("mentionSender").(*types.TypeName); ok {
			if fields, ok := sender.Type().Underlying().(*types.Struct); ok {
				for index := range fields.NumFields() {
					if field := fields.Field(index); field.Name() == "mu" {
						return field
					}
				}
			}
		}
	}
	t.Fatal("the source declares no mentionSender with a mu field: the guard read nothing")
	return nil
}

// lockSiteImporter imports each package as an empty one, so lockSiteProblems type-checks this
// package's own source in every run with nothing built for its imports.
type lockSiteImporter map[string]*types.Package

func (l lockSiteImporter) Import(importPath string) (*types.Package, error) {
	if pkg, ok := l[importPath]; ok {
		return pkg, nil
	}
	pkg := types.NewPackage(importPath, path.Base(importPath))
	pkg.MarkComplete()
	l[importPath] = pkg
	return pkg, nil
}

// parseSources parses sources into positions.
func parseSources(t *testing.T, positions *token.FileSet, sources map[string][]byte) []*ast.File {
	t.Helper()
	var files []*ast.File
	for name, source := range sources {
		file, err := parser.ParseFile(positions, name, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	return files
}
