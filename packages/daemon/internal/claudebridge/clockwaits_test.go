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
// every bound that passes on that clock, but a wait on the wall clock ends on its own, so such a
// row passes a bridge that holds (TestTheBridgeEndsWithAnInboxClientConnected says why it cannot
// see one). This row is the one that sees it: it type-checks the package's non-test files and
// fails by name on each wall-clock wait, found by what each name denotes, so that no import name,
// alias or function value hides one (wallClockWaits):
//   - a reference to time's Sleep, After, AfterFunc, NewTimer, NewTicker or Tick, or to context's
//     WithTimeout, WithDeadline, WithTimeoutCause or WithDeadlineCause, anywhere but in the wall
//     clock's own methods (wallClock, config.go);
//   - a reference to net/http.Client's Timeout field;
//   - a SetDeadline, SetReadDeadline or SetWriteDeadline whose argument is not exactly time.Now()
//     or the zero time.Time{}, the two forms that unblock at once.
//
// The row shows it can fail, in four subtests: each plants one wall-clock wait in a copy of the
// package's own source, in memory, and the wait must be found by name. They are a read deadline
// set 6 s ahead in the inbox's stop path, which holds the bridge's exit for a connected client;
// time imported under another name, with a Sleep in the inbox's close; time.Sleep taken as a value
// and called; and the report's http.Client given back a Timeout.
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
	// The wall clock's own methods are where the bridge reads the wall clock (wallClock).
	var wall [][2]token.Pos
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
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
			if path := object.Pkg().Path(); path == "time" && slices.Contains(timeWaits, object.Name()) || path == "context" && slices.Contains(contextWaits, object.Name()) {
				add(ident.Pos(), "%s.%s waits on the wall clock", path, object.Name())
			}
		case *types.Var:
			if object.IsField() && object.Name() == "Timeout" && isHTTPClientField(object) {
				add(ident.Pos(), "net/http.Client's Timeout bounds a call on the wall clock")
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !slices.Contains(deadlineSets, sel.Sel.Name) {
				return true
			}
			if selection := info.Selections[sel]; selection == nil || selection.Kind() == types.FieldVal {
				return true
			}
			if len(call.Args) != 1 || !unblocksAtOnce(info, call.Args[0]) {
				add(call.Pos(), "%s sets a deadline on the wall clock; only time.Now() or time.Time{}, which unblock at once, may be set", sel.Sel.Name)
			}
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

// isHTTPClientField is whether field is net/http.Client's own field.
func isHTTPClientField(field *types.Var) bool {
	if field.Pkg() == nil || field.Pkg().Path() != "net/http" {
		return false
	}
	client, ok := field.Pkg().Scope().Lookup("Client").(*types.TypeName)
	if !ok {
		return false
	}
	fields, ok := client.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range fields.NumFields() {
		if fields.Field(i) == field {
			return true
		}
	}
	return false
}

// unblocksAtOnce is whether expr is exactly time.Now() or the zero time.Time{}: a deadline that
// has already passed, which ends a blocked read or write at once rather than after a span.
func unblocksAtOnce(info *types.Info, expr ast.Expr) bool {
	switch expr := expr.(type) {
	case *ast.CallExpr:
		if len(expr.Args) != 0 {
			return false
		}
		var name *ast.Ident
		switch fun := expr.Fun.(type) {
		case *ast.Ident:
			name = fun
		case *ast.SelectorExpr:
			name = fun.Sel
		default:
			return false
		}
		fn, ok := info.Uses[name].(*types.Func)
		return ok && fn.Pkg() != nil && fn.Pkg().Path() == "time" && fn.Name() == "Now" && fn.Signature().Recv() == nil
	case *ast.CompositeLit:
		named, ok := info.Types[expr].Type.(*types.Named)
		return ok && len(expr.Elts) == 0 && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "time" && named.Obj().Name() == "Time"
	}
	return false
}
