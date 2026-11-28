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

// specWriterGuardPkg is the package whose every function the walk reads: the check is unexported,
// so every call of it is made in this package.
const specWriterGuardPkg = "api"

// specWriterSinkPkg is the document service's package, whose sinks are methods of its types
// (specWriterSinkTypes). A route's path also calls the package's plain functions, and one of them
// writes a document through a sink: an upload writes its document's text through
// docs.WriteDocumentText (new_document.go), which seeds a new document (SeedText) or replaces the
// text of one that exists (ReplaceText). So the walk reads every function of this package that is
// no method, and every function literal outside a method, wherever the path calls or hands one. It
// enters no method of the package (specWriterSinkMethods): a sink is one, counted where a function
// the walk reads calls it.
const specWriterSinkPkg = "docs"

// specWriterRegistryPkg is the eval registry's package, which writes each project's evals-md
// through the document service's sinks from methods of its own store (evals' createDocument,
// through docs.CreateDocument, and editDocument, through ApplyOps and SnapshotVersion): a project's
// create, a register write and a readings write reach them there. So the walk reads every
// function of this package, its methods included.
const specWriterRegistryPkg = "evals"

// specWriterSinks are the writes of the document service that make or change a version of a
// document, by method name: the nine the spec's Design names, and the accept and the reject of a
// suggestion, which change a document's text too (plan, "Where the plan departs from the spec's
// words"). A call names one through the interface the handlers hold (docs.API) or through the
// service itself (docs.Service), and either is a call of the sink.
var specWriterSinks = []string{
	"AcceptSuggestion", "ApplyOps", "NamedVersion", "RebuildDocument", "RejectSuggestion", "ReplaceText",
	"SeedText", "SetAskBlockText", "SetBlockAttributes", "SnapshotVersion", "WriteIssueAskStates",
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
// the server declares it, so a renamed route leaves no reason pointing at nothing, and its path
// still does not call the check, so an exemption that has stopped being true is deleted rather
// than left to describe the opposite of what the handler does. No test hook reaches a document
// write today: a hook that does goes in with the reason "a test hook, mounted only with
// TestHooksEnabled".
var specWriterExempt = map[string]string{
	"POST /api/v1/issues/{key}/comments":                anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/comments":              anchoredWriteExemption,
	"POST /api/v1/issues/{key}/asks":                    anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/asks":                  anchoredWriteExemption,
	"POST /api/v1/projects":                             evalsDocumentExemption,
	"POST /api/v1/projects/{key}/evals":                 evalsDocumentExemption,
	"POST /api/v1/projects/{key}/evals/{slug}/readings": evalsDocumentExemption,
	"PUT /api/v1/projects/{key}/settings/delivery":      evalsDocumentExemption,
}

// evalsDocumentExemption is why specWriterExempt leaves unchecked the four routes that write a
// document only through the eval registry (specWriterRegistryPkg): a project's create, which seeds
// its evals-md, the register and readings writes, which write an eval's section into it, and a
// project's delivery settings save (dispatch://INFRA-978, R3), which writes each target it holds as
// a new version of that eval's row.
const evalsDocumentExemption = "it writes only the project's evals-md (dispatch://INFRA-2916, R5), a document of the project " +
	"that the eval registry creates and edits (evals' createDocument and editDocument), never an issue's spec or " +
	"Verification document, the two documents the check judges the writer of (spec_writer.go)"

// TestEveryRouteThatWritesADocumentChecksItsWriter holds R1 of INFRA-4764 - on a Legion-run
// issue, a version of the spec or the Verification document from a writer who is no person and no
// architect is refused on every route that writes one - to the server's own route inventory rather
// than to a list of the routes that check today.
//
// The inventory is the route declarations with their test hooks in, read from the source so that
// each route's handler is read with it, and held to the routes the server mounts. A route whose
// path reaches a sink (specWriterSinks) passes when its path also calls the check
// (specWriterCheck), when its declaration states the person scope (routeDecl.OAuthScopes), which
// no worker's credential holds, or when specWriterExempt names it with its reason. Its failure
// names each route and each sink it reaches, with the calls that reach it.
//
// The walk is decisionBlockChecked's, over package api, package evals (specWriterRegistryPkg) and
// the plain functions of package docs (specWriterSinkPkg): every call of every function on the
// path, followed as the copy-lock graph resolves it, plus every function handed to it as an
// argument. It reads each body whole, branches included, so a route reaches every sink of every
// function on its path, and a handler that calls the check under a condition passes here as it
// does in the other static guards of this package; the behavior tests (spec_writer_test.go) hold
// the branches.
func TestEveryRouteThatWritesADocumentChecksItsWriter(t *testing.T) {
	t.Parallel()
	graph := copyLockGraph(t)
	if graph.named[specWriterCheck] == nil {
		t.Errorf("%s, the check every route that writes a checked document calls, is not declared: "+
			"write it (spec_writer.go), or point specWriterCheck at its replacement", specWriterCheck)
	}
	routes := specWriterRoutes(t, graph)
	scopes := declaredScopes()

	// The guard cannot pass by seeing nothing: each sink is a method the document service still
	// has, and some declared route reaches it.
	for _, sink := range specWriterSinks {
		if graph.named["docs.Service."+sink] == nil {
			t.Errorf("docs.Service.%s, a document write the guard looks for, is gone: point specWriterSinks at its replacement", sink)
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(routes)), func(reach specWriterReach) bool { return len(reach.sinks[sink]) > 0 }) {
			t.Errorf("no declared route reaches %s, so the guard sees none of its calls", sink)
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
		case slices.Contains(scopes[name], model.ScopePerson):
			t.Logf("%s: reaches %s; needs the person scope", name, reach.describe())
		case reach.checks:
			t.Logf("%s: reaches %s; checked", name, reach.describe())
		}
	}
	if unchecked := uncheckedSpecWriters(routes, scopes, specWriterExempt); len(unchecked) > 0 {
		t.Errorf("a route writes a document and never checks its writer:\n\t%s\n"+
			"Call refuseSpecWriter on the route's path before its first document call (spec_writer.go), "+
			"declare model.ScopePerson in the route's OAuthScopes (its declaration), or name it in specWriterExempt with the reason it is left.",
			strings.Join(unchecked, "\n\t"))
	}
}

// TestTheSpecWriterExemptionsStillHold holds each route specWriterExempt names to its reason: the
// server declares the route, and its path still does not call the check.
func TestTheSpecWriterExemptionsStillHold(t *testing.T) {
	t.Parallel()
	routes := specWriterRoutes(t, copyLockGraph(t))
	for _, name := range slices.Sorted(maps.Keys(specWriterExempt)) {
		t.Logf("%s: exempt - %s", name, specWriterExempt[name])
	}
	if stale := staleSpecWriterExemptions(routes, specWriterExempt); len(stale) > 0 {
		t.Errorf("an exemption of the spec-writer guard no longer holds:\n\t%s\n"+
			"Delete the route's line from specWriterExempt, or point it at the route that replaced it.",
			strings.Join(stale, "\n\t"))
	}
}

// TestTheSpecWriterGuardSeesAPlantedWriter is the two negative controls of check 9d3e6a10, each on a
// call graph planted from the source below and read by the same helpers the two tests above call:
// the guard names each planted route that reaches ApplyOps with no check, from its own path in
// package api, through a plain function of package docs (as an upload reaches ReplaceText through
// docs.WriteDocumentText) or through a method of package evals (as a register write reaches
// ApplyOps through the registry's editDocument), and the exemption test names a planted exempt
// route whose path calls the check. The planted table also holds a route that checks its writer,
// one that needs the person scope, and one that writes nothing, and neither helper names any of
// them.
func TestTheSpecWriterGuardSeesAPlantedWriter(t *testing.T) {
	t.Parallel()
	graph := plantedSpecWriterGraph(t)
	routes := specWriterReaches(graph, specWriterRouteHandlers(t, graph))
	scopes := map[string]routeScopeSet{"POST /planted/person": {model.ScopePerson}}
	exempt := map[string]string{"POST /planted/exempt": "a planted exemption whose route checks its writer all the same"}

	unchecked := uncheckedSpecWriters(routes, scopes, exempt)
	want := []string{
		"POST /planted/through-docs: reaches ApplyOps (planted/docs/planted.go:",
		"POST /planted/through-evals: reaches ApplyOps (planted/evals/planted.go:",
		"POST /planted/unchecked: reaches ApplyOps (planted/api/planted.go:",
	}
	if !slices.EqualFunc(unchecked, want, strings.HasPrefix) {
		t.Errorf("the guard names %q over the planted table, want the three routes that reach ApplyOps with no check, "+
			"POST /planted/through-docs through a function of package docs, POST /planted/through-evals through a method "+
			"of package evals and POST /planted/unchecked from package api, each naming the call of ApplyOps", unchecked)
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
			stale = append(stale, name+": the server declares no such route")
		case reach.checks:
			stale = append(stale, fmt.Sprintf("%s: its path calls %s now, so its exemption (%s) is no longer true", name, specWriterCheck, exempt[name]))
		}
	}
	return stale
}

// specWriterRoutes is every declared route of the server with what its path runs, held to the
// routes the server mounts with its test hooks in: a route the source read misses would otherwise
// pass unread.
func specWriterRoutes(t *testing.T, graph *lockGraph) map[string]specWriterReach {
	t.Helper()
	handlers := specWriterRouteHandlers(t, graph)
	mounted := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	for _, route := range mounted {
		if _, read := handlers[route.Method+" "+route.Pattern]; !read {
			t.Errorf("the guard reads no %s %s off the route declarations, and the server mounts it", route.Method, route.Pattern)
		}
	}
	if len(handlers) != len(mounted) {
		t.Errorf("the guard reads %d routes off the route declarations, want the %d the server mounts", len(handlers), len(mounted))
	}
	return specWriterReaches(graph, handlers)
}

// specWriterRouteHandlers is every route declaration of graph's package api, by
// "<method> <pattern>", with the handler bodies it names (routeDeclHandlers), and of every
// method: a route of any method may write a document.
func specWriterRouteHandlers(t *testing.T, graph *lockGraph) map[string][]*lockFunction {
	t.Helper()
	routes, _ := routeDeclHandlers(t, graph, func(string) bool { return true })
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		if len(routes[name]) == 0 {
			t.Errorf("the guard cannot read the handler of %s off its declaration", name)
		}
	}
	if len(routes) == 0 {
		t.Fatal("the guard reads no route off the route declarations")
	}
	return routes
}

// declaredScopes is the OAuth scopes each declared route states, by "<method> <pattern>".
func declaredScopes() map[string]routeScopeSet {
	scopes := make(map[string]routeScopeSet, len(routeRegistry))
	for _, route := range routeRegistry {
		scopes[route.Method+" "+route.Pattern] = route.OAuthScopes
	}
	return scopes
}

// specWriterReaches is what each route's path runs, the route's handlers walked by one
// specWriterWalk over graph.
func specWriterReaches(graph *lockGraph, routes map[string][]*lockFunction) map[string]specWriterReach {
	walk := &specWriterWalk{graph: graph, methods: specWriterSinkMethods(graph), steps: map[*lockFunction]*specWriterStep{}}
	reaches := make(map[string]specWriterReach, len(routes))
	for name, handlers := range routes {
		reaches[name] = walk.reach(handlers)
	}
	return reaches
}

// specWriterWalk reads the path of a route, as decisionBlockChecked follows it: every call each
// function makes, resolved as the copy-lock graph resolves it, and every function handed to a call
// as an argument, whatever the callee does with it (lockGraph.calleesAndPassed). It enters every
// function of packages api and evals, and every function of package docs but the ones methods holds
// (specWriterSinkMethods). What one function's own body calls is read once and kept, since most
// routes share most of their path.
type specWriterWalk struct {
	graph   *lockGraph
	methods map[*lockFunction]bool
	steps   map[*lockFunction]*specWriterStep
}

// enters says whether the walk reads fn: a function of package api or of package evals, or a
// function of package docs that is no method of the package and no function literal inside one.
func (w *specWriterWalk) enters(fn *lockFunction) bool {
	return fn != nil && (fn.pkg == specWriterGuardPkg || fn.pkg == specWriterRegistryPkg ||
		(fn.pkg == specWriterSinkPkg && !w.methods[fn]))
}

// specWriterSinkMethods is every method of package docs, with every function literal inside one:
// the document service's own code, which the walk does not enter (specWriterSinkPkg).
func specWriterSinkMethods(graph *lockGraph) map[*lockFunction]bool {
	methods := map[*lockFunction]bool{}
	for object, fn := range graph.declared {
		if fn.pkg != specWriterSinkPkg || object.Signature().Recv() == nil {
			continue
		}
		methods[fn] = true
		ast.Inspect(fn.body, func(node ast.Node) bool {
			if literal, ok := node.(*ast.FuncLit); ok {
				methods[graph.literals[literal]] = true
			}
			return true
		})
	}
	return methods
}

// specWriterStep is what one function's own body runs: the sinks it calls, by method name, with
// where; whether it calls the check; and the functions its calls run that the walk enters.
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
		if !w.enters(fn) || walked[fn] {
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
			if w.enters(callee) && !slices.Contains(step.next, callee) {
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
// import path: a document service with ApplyOps behind an interface, as docs.API holds it, and a
// plain function of the same package that calls it through a function literal, as
// docs.WriteDocumentText calls ReplaceText; a registry whose method writes through that interface,
// as evals.Store's editDocument does; the stand-ins for net/http's method names the declarations
// spell; and a server whose route declarations, each a routeDecl as the server's route files write
// one, hold a route that writes through a function literal with no check, a route that writes
// through the document package's function with no check, one that writes through the registry
// with no check, a route whose path calls the check through a helper before the same write, a
// route that needs the person scope and writes the same way, an exempt route that checks its
// writer all the same, and a route that writes nothing. Each package's name is the one the guard
// reads (docs, evals, http, api), so the graph names its functions as it names the server's.
var plantedSpecWriterSources = map[string]string{
	"planted/docs": `package docs

type API interface {
	ApplyOps(artifactID string) error
}

type Service struct{}

func (s *Service) ApplyOps(artifactID string) error { return nil }

func WriteText(documents API, artifactID string) error {
	return inLedger(func() error { return documents.ApplyOps(artifactID) })
}

func inLedger(write func() error) error { return write() }
`,
	"planted/evals": `package evals

import "planted/docs"

type Store struct{ documents docs.API }

func (s *Store) Register(artifactID string) error { return s.editDocument(artifactID) }

func (s *Store) editDocument(artifactID string) error { return s.documents.ApplyOps(artifactID) }
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
	"planted/evals"
	"planted/http"
)

type routeDecl struct {
	Method, Pattern, Description string
	Handler                      func(*server)
}

var routeRegistry []routeDecl

func declareRoute(route routeDecl) routeDecl {
	routeRegistry = append(routeRegistry, route)
	return route
}

type server struct {
	documents docs.API
	registry  *evals.Store
}

func refuseSpecWriter() error { return nil }

var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/unchecked", Description: "Writes a document and checks no writer.", Handler: (*server).writeUnchecked})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/through-docs", Description: "Writes a document through the document package and checks no writer.", Handler: (*server).writeThroughDocs})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/through-evals", Description: "Writes a document through the registry and checks no writer.", Handler: (*server).writeThroughEvals})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/checked", Description: "Checks its writer, then writes a document.", Handler: (*server).writeChecked})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/person", Description: "Needs the person scope, then writes a document.", Handler: (*server).writeUnchecked})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/exempt", Description: "Is exempt, and checks its writer all the same.", Handler: (*server).writeChecked})
var _ = declareRoute(routeDecl{Method: http.MethodGet, Pattern: "/planted/read", Description: "Writes nothing.", Handler: (*server).read})
var _ = declareRoute(routeDecl{Method: http.MethodPost, Pattern: "/planted/nothing", Description: "Writes nothing either.", Handler: (*server).read})

func (s *server) writeUnchecked() {
	s.inTransaction(func() error { return s.documents.ApplyOps("spec") })
}

func (s *server) writeThroughDocs() { _ = docs.WriteText(s.documents, "spec") }

func (s *server) writeThroughEvals() { _ = s.registry.Register("evals-md") }

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
