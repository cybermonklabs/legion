//go:build !race

// The guard is static, as the copy-lock, keyed-write and decision-block guards whose graph it
// reads are: it reads the module's source, so the race detector adds nothing to it, and leaving it
// out of the -race run spares that job type-checking the server a second time.

package api

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// specWriterCheck is the check of who writes a version of a Legion-run issue's spec or
// Verification document (spec_writer.go), written as the call graph names a function: its
// package's name, then its own.
const specWriterCheck = "api.refuseSpecWriter"

// specWriterGuardPkg is the package the walk reads: the check is unexported, so every path to it
// is inside this package, and every call of a sink a route makes is made from it.
const specWriterGuardPkg = "api"

// specWriterSinks are the writes of the document service that make or change a version of a
// document, by method name: the nine the spec's Design names, with KeepAskBlockInStep in place of
// its SetBlockAttributes, which KeepAskBlockInStep replaced as the answer, resolve and decline
// routes' write of an ask's state into the ask's block; and the accept and the reject of a
// suggestion, which change a document's text too (plan, "Where the plan departs from the spec's
// words"). A call names one through the interface the handlers hold (docs.API) or through the
// service itself (docs.Service), and either is a call of the sink.
var specWriterSinks = []string{
	"AcceptSuggestion", "ApplyOps", "KeepAskBlockInStep", "NamedVersion", "RebuildDocument",
	"RejectSuggestion", "ReplaceText", "SeedText", "SetAskBlockText", "SnapshotVersion",
	"WriteIssueAskStates",
}

// specWriterSinkTypes are the two types a sink is called through, as the graph names them.
var specWriterSinkTypes = []string{"docs.API.", "docs.Service."}

// anchoredWriteExemption is why specWriterExempt leaves the four anchored comment and ask routes
// unchecked (dispatch://INFRA-4764/spec, Design, "The two writes R1 leaves as they are").
const anchoredWriteExemption = "an anchored comment, ask or suggestion writes a version (MarkQuote and SnapshotVersion, anchors.go) " +
	"that adds only its writer's mark around words others wrote, and a comment or a suggestion is how a worker " +
	"proposes a change to the architect; dispatch suggest posts on the two comment routes " +
	"(packages/envoy-client/src/dispatch-commands/conversation.ts)"

// specWriterExempt names every route that reaches a document write and is left unchecked, with
// the reason it is left. A route here is held to two things (TestTheSpecWriterExemptionsStillHold):
// the table serves it, so a renamed row leaves no reason pointing at nothing, and its path still
// does not call the check, so an exemption that has stopped being true is deleted rather than left
// to describe the opposite of what the handler does. No test hook reaches a document write today:
// a hook that does goes in with the reason "a test hook, mounted only with TestHooksEnabled".
var specWriterExempt = map[string]string{
	"POST /api/v1/issues/{key}/comments":   anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/comments": anchoredWriteExemption,
	"POST /api/v1/issues/{key}/asks":       anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/asks":     anchoredWriteExemption,
}

// TestEveryRouteThatWritesADocumentChecksItsWriter holds R1 of INFRA-4764 - on a Legion-run
// issue, a version of the spec or the Verification document from a writer who is no person and no
// architect is refused on every route that writes one - to the server's own route inventory rather
// than to a list of the routes that check today.
//
// The inventory is the table (routes_table.go) with its test hooks in, read from the source so that
// each row's handler is read with it, and held to the table the server mounts. A row whose path
// reaches a sink (specWriterSinks) passes when its path also calls the check (specWriterCheck),
// when its row declares the person scope in routeScopes, which no worker's credential holds, or
// when specWriterExempt names it with its reason. Its failure names each route and each sink it
// reaches, with the calls that reach it.
//
// The walk is decisionBlockChecked's: every call of every function on the path inside package api,
// followed as the copy-lock graph resolves it, plus every function handed to it as an argument. It
// reads each body whole, branches included, so a route reaches every sink of every function on its
// path, and a handler that calls the check under a condition passes here as it does in the other
// static guards of this package; the behavior tests (spec_writer_test.go) hold the branches.
func TestEveryRouteThatWritesADocumentChecksItsWriter(t *testing.T) {
	graph := copyLockGraph(t)
	if graph.named[specWriterCheck] == nil {
		t.Errorf("%s, the check every route that writes a checked document calls, is not declared: "+
			"write it (spec_writer.go), or point specWriterCheck at its replacement", specWriterCheck)
	}
	routes := specWriterRoutes(t, graph)

	// The guard cannot pass by seeing nothing: each sink is a method the document service still
	// has, and some route of the table reaches it.
	for _, sink := range specWriterSinks {
		if graph.named["docs.Service."+sink] == nil {
			t.Errorf("docs.Service.%s, a document write the guard looks for, is gone: point specWriterSinks at its replacement", sink)
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(routes)), func(reach specWriterReach) bool { return len(reach.sinks[sink]) > 0 }) {
			t.Errorf("no route of the table reaches %s, so the guard sees none of its calls", sink)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(routes)) {
		reach := routes[name]
		if len(reach.sinks) == 0 {
			continue
		}
		reason, exempt := specWriterExempt[name]
		switch {
		case exempt:
			t.Logf("%s: reaches %s; exempt - %s", name, reach.describe(), reason)
		case slices.Contains(routeScopes[name], model.ScopePerson):
			t.Logf("%s: reaches %s; needs the person scope", name, reach.describe())
		case reach.checks:
			t.Logf("%s: reaches %s; checked", name, reach.describe())
		}
	}
	if unchecked := uncheckedSpecWriters(routes, routeScopes, specWriterExempt); len(unchecked) > 0 {
		t.Errorf("a route writes a document and never checks its writer:\n\t%s\n"+
			"Call refuseSpecWriter on the route's path before its first document call (spec_writer.go), "+
			"declare model.ScopePerson for the route in routeScopes, or name it in specWriterExempt with the reason it is left.",
			strings.Join(unchecked, "\n\t"))
	}
}

// TestTheSpecWriterExemptionsStillHold holds each route specWriterExempt names to its reason: the
// table serves the route, and its path still does not call the check.
func TestTheSpecWriterExemptionsStillHold(t *testing.T) {
	routes := specWriterRoutes(t, copyLockGraph(t))
	for _, name := range slices.Sorted(maps.Keys(specWriterExempt)) {
		t.Logf("%s: exempt - %s", name, specWriterExempt[name])
	}
	if stale := staleSpecWriterExemptions(routes, specWriterExempt); len(stale) > 0 {
		t.Errorf("an exemption of the spec-writer guard no longer holds:\n\t%s\n"+
			"Delete the route's line from specWriterExempt, or point it at the row that replaced it.",
			strings.Join(stale, "\n\t"))
	}
}

// TestTheSpecWriterGuardSeesAPlantedWriter is the two negative controls of check 9d3e6a10, each on a
// call graph planted from the source below and read by the same helpers the two tests above call:
// the guard names a planted route that reaches ApplyOps with no check, and the exemption test names
// a planted exempt route whose path calls the check. The planted table also holds a route that
// checks its writer, one that needs the person scope, and one that writes nothing, and neither
// helper names any of them.
func TestTheSpecWriterGuardSeesAPlantedWriter(t *testing.T) {
	graph := plantedSpecWriterGraph(t)
	routes := specWriterReaches(graph, specWriterRouteHandlers(t, graph))
	scopes := map[string]routeScopeSet{"POST /planted/person": {model.ScopePerson}}
	exempt := map[string]string{"POST /planted/exempt": "a planted exemption whose route checks its writer all the same"}

	unchecked := uncheckedSpecWriters(routes, scopes, exempt)
	if len(unchecked) != 1 || !strings.HasPrefix(unchecked[0], "POST /planted/unchecked: reaches ApplyOps (") {
		t.Errorf("the guard names %q over the planted table, want the one route that reaches ApplyOps with no check, "+
			"POST /planted/unchecked, naming ApplyOps", unchecked)
	}
	stale := staleSpecWriterExemptions(routes, exempt)
	if len(stale) != 1 || !strings.HasPrefix(stale[0], "POST /planted/exempt: its path calls "+specWriterCheck) {
		t.Errorf("the exemption test names %q over the planted table, want the one exempt route whose path calls %s, "+
			"POST /planted/exempt", stale, specWriterCheck)
	}
}

// specWriterReach is what one route's path runs: each sink it reaches, by method name, with every
// call on the path that reaches it ("<file>:<line>", sorted), and whether it calls the check.
type specWriterReach struct {
	sinks  map[string][]string
	checks bool
}

// describe names each sink reach reaches, in order, with the calls that reach it.
func (reach specWriterReach) describe() string {
	named := make([]string, 0, len(reach.sinks))
	for _, sink := range slices.Sorted(maps.Keys(reach.sinks)) {
		named = append(named, sink+" ("+strings.Join(reach.sinks[sink], ", ")+")")
	}
	return strings.Join(named, ", ")
}

// uncheckedSpecWriters is the guard's decision over routes, by "<method> <pattern>": each route
// that reaches a sink and neither calls the check, nor declares the person scope in scopes, nor is
// named in exempt, with the sinks it reaches. TestEveryRouteThatWritesADocumentChecksItsWriter
// runs it over the server's table and TestTheSpecWriterGuardSeesAPlantedWriter over a planted one.
func uncheckedSpecWriters(routes map[string]specWriterReach, scopes map[string]routeScopeSet, exempt map[string]string) []string {
	var unchecked []string
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		reach := routes[name]
		if len(reach.sinks) == 0 || reach.checks || slices.Contains(scopes[name], model.ScopePerson) {
			continue
		}
		if _, exempted := exempt[name]; exempted {
			continue
		}
		unchecked = append(unchecked, name+": reaches "+reach.describe())
	}
	return unchecked
}

// staleSpecWriterExemptions is the exemption test's decision over routes: each route exempt names
// that routes does not hold, and each whose path calls the check, with the reason it was exempt.
// TestTheSpecWriterExemptionsStillHold runs it over the server's table and
// TestTheSpecWriterGuardSeesAPlantedWriter over a planted one.
func staleSpecWriterExemptions(routes map[string]specWriterReach, exempt map[string]string) []string {
	var stale []string
	for _, name := range slices.Sorted(maps.Keys(exempt)) {
		reach, served := routes[name]
		switch {
		case !served:
			stale = append(stale, name+": the table serves no such route")
		case reach.checks:
			stale = append(stale, fmt.Sprintf("%s: its path calls %s now, so its exemption (%s) is no longer true", name, specWriterCheck, exempt[name]))
		}
	}
	return stale
}

// specWriterRoutes is every route of the server's table with what its path runs, held to the table
// the server mounts with its test hooks in: a row the source read misses would otherwise pass
// unread.
func specWriterRoutes(t *testing.T, graph *lockGraph) map[string]specWriterReach {
	t.Helper()
	handlers := specWriterRouteHandlers(t, graph)
	mounted := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	for _, route := range mounted {
		if _, read := handlers[route.Method+" "+route.Pattern]; !read {
			t.Errorf("the guard reads no %s %s off the source of the routes table, which mounts it", route.Method, route.Pattern)
		}
	}
	if len(handlers) != len(mounted) {
		t.Errorf("the guard reads %d routes off the source of the routes table, want the %d it mounts", len(handlers), len(mounted))
	}
	return specWriterReaches(graph, handlers)
}

// specWriterRouteHandlers is every row of graph's routes table, by "<method> <pattern>", with the
// handler bodies that row mounts (routeTableHandlers), and of every method: a route of any method
// may write a document.
func specWriterRouteHandlers(t *testing.T, graph *lockGraph) map[string][]*lockFunction {
	t.Helper()
	routes := routeTableHandlers(t, graph, func(string) bool { return true })
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		if len(routes[name]) == 0 {
			t.Errorf("the guard cannot read the handler of %s off the table", name)
		}
	}
	if len(routes) == 0 {
		t.Fatal("the guard reads no row off the routes table")
	}
	return routes
}

// specWriterReaches is what each route's path runs, the route's handlers walked by one
// specWriterWalk over graph.
func specWriterReaches(graph *lockGraph, routes map[string][]*lockFunction) map[string]specWriterReach {
	walk := &specWriterWalk{graph: graph, steps: map[*lockFunction]*specWriterStep{}}
	reaches := make(map[string]specWriterReach, len(routes))
	for name, handlers := range routes {
		reaches[name] = walk.reach(handlers)
	}
	return reaches
}

// specWriterWalk reads the path of a route inside package api, as decisionBlockChecked follows it:
// every call each function makes, resolved as the copy-lock graph resolves it, and every function
// handed to a call as an argument, whatever the callee does with it (lockGraph.calleesAndPassed).
// What one function's own body calls is read once and kept, since most routes share most of their
// path.
type specWriterWalk struct {
	graph *lockGraph
	steps map[*lockFunction]*specWriterStep
}

// specWriterStep is what one function's own body runs: the sinks it calls, by method name, with
// where; whether it calls the check; and the functions of package api its calls run.
type specWriterStep struct {
	sinks  map[string][]string
	checks bool
	next   []*lockFunction
}

// reach is what running any of handlers runs.
func (w *specWriterWalk) reach(handlers []*lockFunction) specWriterReach {
	reach := specWriterReach{sinks: map[string][]string{}}
	walked := map[*lockFunction]bool{}
	var walk func(fn *lockFunction)
	walk = func(fn *lockFunction) {
		if fn == nil || fn.pkg != specWriterGuardPkg || walked[fn] {
			return
		}
		walked[fn] = true
		step := w.step(fn)
		reach.checks = reach.checks || step.checks
		for sink, calls := range step.sinks {
			for _, call := range calls {
				if !slices.Contains(reach.sinks[sink], call) {
					reach.sinks[sink] = append(reach.sinks[sink], call)
				}
			}
		}
		for _, next := range step.next {
			walk(next)
		}
	}
	for _, handler := range handlers {
		walk(handler)
	}
	for sink := range reach.sinks {
		slices.Sort(reach.sinks[sink])
	}
	return reach
}

// step reads fn's own body once (ownCalls, a function literal inside it being a function of its
// own that runs where it is called or passed).
func (w *specWriterWalk) step(fn *lockFunction) *specWriterStep {
	if step, read := w.steps[fn]; read {
		return step
	}
	step := &specWriterStep{sinks: map[string][]string{}}
	w.steps[fn] = step
	for _, call := range ownCalls(fn) {
		name := calledFunctionName(w.graph, call)
		if name == specWriterCheck {
			step.checks = true
		}
		if sink, isSink := specWriterSinkOf(name); isSink {
			step.sinks[sink] = append(step.sinks[sink], w.graph.line(fn.source, call))
		}
		for _, callee := range w.graph.calleesAndPassed(call) {
			if callee != nil && callee.pkg == specWriterGuardPkg && !slices.Contains(step.next, callee) {
				step.next = append(step.next, callee)
			}
		}
	}
	return step
}

// specWriterSinkOf is the sink name names, as calledFunctionName gives a call's function, and
// whether it is one.
func specWriterSinkOf(name string) (string, bool) {
	for _, through := range specWriterSinkTypes {
		if method, called := strings.CutPrefix(name, through); called && slices.Contains(specWriterSinks, method) {
			return method, true
		}
	}
	return "", false
}

// plantedSpecWriterSources is the planted module TestTheSpecWriterGuardSeesAPlantedWriter reads, by
// import path: a document service with ApplyOps behind an interface, as docs.API holds it; the
// stand-ins for net/http's method names the table's rows spell; and a server whose routes table
// holds a route that writes through a function literal with no check, a route whose path calls the
// check through a helper before the same write, a route that needs the person scope and writes the
// same way, an exempt route that checks its writer all the same, and a route that writes nothing.
// Each package's name is the one the guard reads (docs, http, api), so the graph names its
// functions as it names the server's.
var plantedSpecWriterSources = map[string]string{
	"planted/docs": `package docs

type API interface {
	ApplyOps(artifactID string) error
}

type Service struct{}

func (s *Service) ApplyOps(artifactID string) error { return nil }
`,
	"planted/http": `package http

const (
	MethodGet  = "GET"
	MethodPost = "POST"
)
`,
	"planted/api": `package api

import (
	"planted/docs"
	"planted/http"
)

type routeRow struct {
	Method, Pattern, Auth, Description string
	Handler                            func()
}

type server struct{ documents docs.API }

func refuseSpecWriter() error { return nil }

func (s *server) routes() []routeRow {
	return []routeRow{
		{http.MethodPost, "/planted/unchecked", "any", "Writes a document and checks no writer.", s.writeUnchecked},
		{http.MethodPost, "/planted/checked", "any", "Checks its writer, then writes a document.", s.writeChecked},
		{http.MethodPost, "/planted/person", "any", "Needs the person scope, then writes a document.", s.writeUnchecked},
		{http.MethodPost, "/planted/exempt", "any", "Is exempt, and checks its writer all the same.", s.writeChecked},
		{http.MethodGet, "/planted/read", "any", "Writes nothing.", s.read},
		{http.MethodPost, "/planted/nothing", "any", "Writes nothing either.", s.read},
	}
}

func (s *server) writeUnchecked() {
	s.inTransaction(func() error { return s.documents.ApplyOps("spec") })
}

func (s *server) writeChecked() {
	if s.checkWriter() != nil {
		return
	}
	s.writeUnchecked()
}

func (s *server) checkWriter() error { return refuseSpecWriter() }

func (s *server) inTransaction(write func() error) { _ = write() }

func (s *server) read() {}
`,
}

// plantedImports refuses every import from outside the planted module, which imports none.
type plantedImports struct{}

func (plantedImports) Import(path string) (*types.Package, error) {
	return nil, fmt.Errorf("the planted module imports nothing from outside it, not %s", path)
}

// plantedSpecWriterGraph type-checks plantedSpecWriterSources, written into a directory of the
// test's own, through the copy-lock guard's importer, and builds the graph over it as
// buildCopyLockGraph builds the server's (newLockGraph): the same functions, literals, stored
// functions and interface targets, without the locks, which the spec-writer guard does not read.
func plantedSpecWriterGraph(t *testing.T) *lockGraph {
	t.Helper()
	root := t.TempDir()
	listed := map[string]listedPackage{}
	for path, source := range plantedSpecWriterSources {
		dir := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "planted.go"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		listed[path] = listedPackage{ImportPath: path, Dir: dir, GoFiles: []string{"planted.go"}, Module: &struct{ Main bool }{Main: true}}
	}
	modules := &moduleImporter{
		fset:     token.NewFileSet(),
		listed:   listed,
		exported: plantedImports{},
		checked:  map[string]*types.Package{},
		info: &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		},
	}
	if _, err := modules.Import("planted/api"); err != nil {
		t.Fatalf("type-check the planted module: %v", err)
	}
	if len(modules.problems) > 0 {
		t.Fatalf("type-check the planted module: %v", modules.problems)
	}
	constants, err := stringConstants(modules.sources)
	if err != nil {
		t.Fatal(err)
	}
	return newLockGraph(root, modules, constants)
}
