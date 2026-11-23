package claudebridge

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testtypes"
)

// The bridge waits on nothing but its Config.Clock. A row that reads the stop on a stopClock sees
// every bound that passes on that clock, and a wall-clock wait it fails at run time only by the
// state the wait leaves the bridge's goroutines in (TestTheBridgeEndsWithAnInboxClientConnected,
// heldOnTheWallClock). This row finds the waits in the source: it type-checks the package's
// non-test files and fails by name on each wall-clock wait, found by what each name denotes, so
// that no import name, alias, function value or method value hides one (wallClockWaits):
//   - a reference to time's Sleep, After, AfterFunc, NewTimer, NewTicker or Tick, to context's
//     WithTimeout, WithDeadline, WithTimeoutCause or WithDeadlineCause, or to a sleep or a wait
//     with a timeout of syscall's or golang.org/x/sys/unix's (Nanosleep, Select, Poll and their
//     kin), anywhere but in the wall clock's own After (wallClock, config.go);
//   - a reference to a timeout field of net's or net/http's (http.Client's Timeout,
//     http.Transport's ResponseHeaderTimeout and its kin, net.Dialer's Timeout and Deadline);
//   - a reference to a SetDeadline, SetReadDeadline or SetWriteDeadline method other than a call
//     with exactly time.Now(), the one form that unblocks at once: a later time waits on the wall
//     clock, the zero time.Time{} clears the deadline so the call waits without bound, and a
//     method value or method expression can be called with anything.
//
// The row shows it can fail, in eight subtests: each plants one wall-clock wait in a copy of the
// package's own source, in memory, and the wait must be found by name. They are a read deadline
// set 6 s ahead in the inbox's stop path, which holds the bridge's exit for a connected client;
// time imported under another name, with a Sleep in the inbox's close; time.Sleep taken as a
// value and called; the report's http.Client given back a Timeout; the read deadline set through
// a method value; a second method of the wall clock's that sleeps, called from the inbox's close;
// syscall.Nanosleep in the inbox's close; and the report's client given a Transport with a
// ResponseHeaderTimeout.
func TestTheBridgeWaitsOnlyOnItsClock(t *testing.T) {
	sources := bridgeSources(t)
	for _, found := range wallClockWaits(t, sources) {
		t.Errorf("%s; the bridge's waits run on its Config.Clock, so a row on a stopClock sees them", found)
	}
	type replace struct{ file, old, new string }
	for _, plant := range []struct {
		name     string
		replaces []replace
		want     string
	}{
		{name: "a read deadline set ahead in the inbox's stop path", want: "SetReadDeadline",
			replaces: []replace{{"inbox.go", "_ = conn.SetReadDeadline(time.Now())", "_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))"}}},
		{name: "time imported under another name", want: "time.Sleep",
			replaces: []replace{
				{"inbox.go", "import (\n", "import (\n\twall \"time\"\n"},
				{"inbox.go", "func (in *inbox) close() {\n", "func (in *inbox) close() {\n\twall.Sleep(6 * wall.Second)\n"},
			}},
		{name: "time.Sleep taken as a value", want: "time.Sleep",
			replaces: []replace{{"inbox.go", "func (in *inbox) close() {\n", "func (in *inbox) close() {\n\tsleep := time.Sleep\n\tsleep(6 * time.Second)\n"}}},
		{name: "the report's http.Client given a Timeout", want: "net/http.Client's Timeout",
			replaces: []replace{{"turnoutcome.go", "client:     &http.Client{},", "client:     &http.Client{Timeout: reportTimeout},"}}},
		{name: "a read deadline set through a method value", want: "SetReadDeadline",
			replaces: []replace{{"inbox.go", "_ = conn.SetReadDeadline(time.Now())", "setDeadline := conn.SetReadDeadline\n\t\t_ = setDeadline(time.Now().Add(6 * time.Second))"}}},
		{name: "a second method of the wall clock's that sleeps", want: "time.Sleep",
			replaces: []replace{
				{"config.go", "func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }\n", "func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }\n\nfunc (wallClock) hold() { time.Sleep(6 * time.Second) }\n"},
				{"inbox.go", "func (in *inbox) close() {\n", "func (in *inbox) close() {\n\twallClock{}.hold()\n"},
			}},
		{name: "syscall.Nanosleep in the inbox's close", want: "syscall.Nanosleep",
			replaces: []replace{
				{"inbox.go", "import (\n", "import (\n\tsleepsys \"syscall\"\n"},
				{"inbox.go", "func (in *inbox) close() {\n", "func (in *inbox) close() {\n\t_ = sleepsys.Nanosleep(&sleepsys.Timespec{Sec: 6}, nil)\n"},
			}},
		{name: "the report's client given a Transport with a ResponseHeaderTimeout", want: "net/http.Transport's ResponseHeaderTimeout",
			replaces: []replace{{"turnoutcome.go", "client:     &http.Client{},", "client:     &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: reportTimeout}},"}}},
	} {
		t.Run(plant.name, func(t *testing.T) {
			planted := maps.Clone(sources)
			for _, r := range plant.replaces {
				if n := strings.Count(planted[r.file], r.old); n != 1 {
					t.Fatalf("%s holds %q %d times, want once: the plant cannot be made, so this row cannot show it would find it", r.file, r.old, n)
				}
				planted[r.file] = strings.Replace(planted[r.file], r.old, r.new, 1)
			}
			found := wallClockWaits(t, planted)
			if !slices.ContainsFunc(found, func(f string) bool { return strings.Contains(f, plant.want) }) {
				t.Errorf("the plant (%s) was not found as %q: this row would pass a bridge that holds on the wall clock; it found %q", plant.name, plant.want, found)
			}
		})
	}
}

// bridgeSources is the text of each non-test Go file the package builds on this platform (its
// build constraints, build.Default.MatchFile), by its name, read from the directory the test runs
// in, which is the package's own.
func bridgeSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if match, err := build.Default.MatchFile(".", name); err != nil {
			t.Fatal(err)
		} else if !match {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name] = string(raw)
	}
	if len(sources) == 0 {
		t.Fatal("no non-test file of the package was read: the row would see nothing")
	}
	return sources
}

var (
	timeWaits    = []string{"Sleep", "After", "AfterFunc", "NewTimer", "NewTicker", "Tick"}
	contextWaits = []string{"WithTimeout", "WithDeadline", "WithTimeoutCause", "WithDeadlineCause"}
	// syscallWaits are the sleeps and the waits with a timeout of their own in syscall and
	// golang.org/x/sys/unix.
	syscallWaits = []string{"Nanosleep", "ClockNanosleep", "Select", "Pselect", "Poll", "Ppoll", "EpollWait", "Pause"}
	deadlineSets = []string{"SetDeadline", "SetReadDeadline", "SetWriteDeadline"}
)

// wallClockWaits type-checks sources as this package and answers each wall-clock wait in them,
// "<file>:<line>:<column>: <what>", in order: every place TestTheBridgeWaitsOnlyOnItsClock names.
// The types of what the package imports are read from where the compiler left them
// (testtypes.CompiledDependencies).
func wallClockWaits(t *testing.T, sources map[string]string) []string {
	t.Helper()
	compiled, err := testtypes.CompiledDependencies("github.com/sjawhar/legion/daemon/internal/claudebridge")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, name, sources[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	info := &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "gc", compiled)}
	if _, err := config.Check("github.com/sjawhar/legion/daemon/internal/claudebridge", fset, files, info); err != nil {
		t.Fatalf("type-check the package's source: %v", err)
	}
	// The wall clock's After is where the bridge reads the wall clock (wallClock); no other method
	// of the wall clock's is.
	var wall [][2]token.Pos
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "After" {
				continue
			}
			method, ok := info.Defs[fn.Name].(*types.Func)
			if !ok {
				continue
			}
			recv := method.Signature().Recv().Type()
			if pointer, ok := recv.(*types.Pointer); ok {
				recv = pointer.Elem()
			}
			if named, ok := recv.(*types.Named); ok && named.Obj().Name() == "wallClock" {
				wall = append(wall, [2]token.Pos{fn.Pos(), fn.End()})
			}
		}
	}
	inWallClock := func(pos token.Pos) bool {
		return slices.ContainsFunc(wall, func(r [2]token.Pos) bool { return pos >= r[0] && pos < r[1] })
	}
	var found []struct {
		pos  token.Pos
		text string
	}
	add := func(pos token.Pos, format string, args ...any) {
		if !inWallClock(pos) {
			found = append(found, struct {
				pos  token.Pos
				text string
			}{pos, fmt.Sprintf(format, args...)})
		}
	}
	for ident, object := range info.Uses {
		switch object := object.(type) {
		case *types.Func:
			if object.Pkg() == nil || object.Signature().Recv() != nil {
				continue
			}
			path := object.Pkg().Path()
			if path == "time" && slices.Contains(timeWaits, object.Name()) || path == "context" && slices.Contains(contextWaits, object.Name()) ||
				(path == "syscall" || path == "golang.org/x/sys/unix") && slices.Contains(syscallWaits, object.Name()) {
				add(ident.Pos(), "%s.%s waits on the wall clock", path, object.Name())
			}
		case *types.Var:
			if owner, ok := netTimeoutField(object); ok {
				add(ident.Pos(), "%s's %s bounds a call on the wall clock", owner, object.Name())
			}
		}
	}
	// A deadline method is allowed only where it is called at once with exactly time.Now(): a
	// deadline already passed, which ends a blocked read or write at once. Any other argument sets
	// a span on the wall clock, the zero time.Time{} included, which clears the deadline so the
	// read or write waits without bound; and a method value or method expression of one can be
	// called with anything.
	allowed := map[*ast.SelectorExpr]bool{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && len(call.Args) == 1 && unblocksAtOnce(info, call.Args[0]) {
					allowed[sel] = true
				}
			}
			return true
		})
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !slices.Contains(deadlineSets, sel.Sel.Name) || allowed[sel] {
				return true
			}
			if selection := info.Selections[sel]; selection == nil || selection.Kind() == types.FieldVal {
				return true
			}
			add(sel.Pos(), "%s sets a deadline on the wall clock; only a call with exactly time.Now(), which unblocks at once, may be made", sel.Sel.Name)
			return true
		})
	}
	slices.SortFunc(found, func(a, b struct {
		pos  token.Pos
		text string
	}) int {
		return int(a.pos - b.pos)
	})
	texts := make([]string, 0, len(found))
	for _, f := range found {
		texts = append(texts, fmt.Sprintf("%s: %s", fset.Position(f.pos), f.text))
	}
	return texts
}

// netTimeoutField is the type that owns field, "<package>.<type>", where field is one of net's or
// net/http's timeout fields: one named Timeout or Deadline, or ending in Timeout (http.Client's
// Timeout, http.Transport's ResponseHeaderTimeout and its kin, http.Server's, net.Dialer's).
func netTimeoutField(field *types.Var) (string, bool) {
	if !field.IsField() || field.Pkg() == nil {
		return "", false
	}
	path, name := field.Pkg().Path(), field.Name()
	if path != "net" && path != "net/http" || name != "Deadline" && !strings.HasSuffix(name, "Timeout") {
		return "", false
	}
	scope := field.Pkg().Scope()
	for _, typeName := range scope.Names() {
		owner, ok := scope.Lookup(typeName).(*types.TypeName)
		if !ok {
			continue
		}
		fields, ok := owner.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := range fields.NumFields() {
			if fields.Field(i) == field {
				return path + "." + typeName, true
			}
		}
	}
	return "", false
}

// unblocksAtOnce is whether expr is exactly time.Now(): a deadline that has already passed, which
// ends a blocked read or write at once rather than after a span.
func unblocksAtOnce(info *types.Info, expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	var name *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun
	case *ast.SelectorExpr:
		name = fun.Sel
	default:
		return false
	}
	fn, ok := info.Uses[name].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "time" && fn.Name() == "Now" && fn.Signature().Recv() == nil
}
