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
// document, by method name: the nine the spec's Design names, and the accept and the reject of a
// suggestion, which change a document's text too (plan, "Where the plan departs from the spec's
// words"). KeepAskBlockInStep is the write of a closed ask's block, which took the place of
// SetBlockAttributes once a person's answer lives on the ask's node. A call names one through the
// interface the handlers hold (docs.API) or through the service itself (docs.Service), and either
// is a call of the sink. DrainOwedQueries is no sink: the version it appends once a check result
// is recorded or retracted re-renders the issue's query documents for that result, and a result
// is no requirement, criterion or check node, nor is its tuple to its check a relation out of
// one, so the version writes nothing the check guards; specWriterRevisions names the routes that
// reach it, each with that reason.
var specWriterSinks = []string{
	"AcceptSuggestion", "ApplyOps", "KeepAskBlockInStep", "NamedVersion", "RebuildDocument", "RejectSuggestion",
	"ReplaceText", "SeedText", "SetAskBlockText", "SnapshotVersion", "WriteIssueAskStates",
}

// specWriterSinkTypes are the two types a sink is called through, as the graph names them.
var specWriterSinkTypes = []string{"docs.API.", "docs.Service."}

// specWriterNodeSinks are the writes of package api that write a node's state and a pointer out of
// a node, as the graph names them: the one write path of the state route and the settle
// (writeNodeState), and of the pointer route and the pre-mortem conversion's moved_to
// (writeReasoningPointer). Each writes a requirement, criterion or check node, or a relation out
// of one, where its node is one, which only a person or an architect writes on an issue Legion
// runs (refuseSpecWriterOfNode), so the guard counts a call of either as it counts a call of a sink
// of the document service: a route that reaches one and does not call the check fails, naming the
// route and the calls that reach it.
var specWriterNodeSinks = []string{"api.writeNodeState", "api.writeReasoningPointer"}

// specWriterRevision is the write of the document service that appends a version of each query
// document of an issue whose record a write moved and marked owed (docs.Service.DrainOwedQueries),
// by method name, called through either of specWriterSinkTypes. It is no sink (specWriterSinks),
// and the routes whose path reaches it are the ones specWriterRevisions names.
const specWriterRevision = "DrainOwedQueries"

// resultRevisionExemption is why specWriterRevisions leaves the two result routes' versions
// unchecked.
const resultRevisionExemption = "a result recorded or retracted moves the record of its check's Verification query, and the " +
	"version DrainOwedQueries appends re-renders that document from the store and writes no requirement, criterion or check " +
	"node and no value or relation out of one: a result is none of those nodes, and its tuple to its check is a relation " +
	"out of the result (TestARecordedResultAndItsRetractionChangeNoRequirementCriterionOrCheck)"

// nodeMoveRevisionExemption is why specWriterRevisions leaves the versions the node PATCH's drain
// appends unchecked.
const nodeMoveRevisionExemption = "a node move checks its writer before it moves anything (refuseSpecWriterOfNode in " +
	"moveNode, node_move.go): a requirement, criterion or check moves, and so has a relation out of it written, only for " +
	"a person or an architect of the issue's tree; the version DrainOwedQueries then appends re-renders each query " +
	"document whose record the move changed from the store, and writes no node and no relation beyond the move the " +
	"check admitted"

// nodeDeleteRevisionExemption is why specWriterRevisions leaves the versions the node DELETE's
// drain appends unchecked.
const nodeDeleteRevisionExemption = "a node delete checks its writer before it takes anything (refuseSpecWriterOfNode in " +
	"node_delete.go, once for each issue and document the subtree holds): a requirement, criterion or check is taken, " +
	"and so has the relations out of it retracted, only for a person or an architect of the issue's tree; the version " +
	"DrainOwedQueries then appends re-renders each query document whose record the delete changed from the store, and " +
	"writes no node and no relation beyond the delete the check admitted"

// specWriterRevisions names every route whose path reaches specWriterRevision, with the reason
// the version it appends there is left unchecked. TestEveryRouteThatWritesADocumentChecksItsWriter
// holds the table to it both ways (unheldQueryRevisions): a route that reaches the revision and is
// not named here fails, naming it, and so does a route named here that the table does not serve or
// whose path no longer reaches the revision, so no reason is left describing a route it does not.
//
// Two drains are outside the walk, which reads package api alone: the one an issue room's
// settlement runs once it has committed (commitVersion, docs/settlement.go), of the query documents
// a settled edit moved, and the block-index tick's, of the marks a request or a settlement left
// (drainOwedQueryMarks, docs/query_marks.go). Neither needs a writer check of its own: what a
// settlement settles reached the room through a document socket, which admits a person alone
// (requestActor, docs/socket_admission.go), or through a route whose own path this test holds to
// the check, and every other mark is left by one of the routes specWriterRevisions names.
var specWriterRevisions = map[string]string{
	"POST /api/v1/issues/{key}/results":      resultRevisionExemption,
	"POST /api/v1/results/{id}/retract":      resultRevisionExemption,
	"PATCH /api/v1/issues/{key}/nodes/{id}":  nodeMoveRevisionExemption,
	"DELETE /api/v1/issues/{key}/nodes/{id}": nodeDeleteRevisionExemption,
}

// anchoredWriteExemption is why specWriterExempt leaves the four anchored comment and ask routes
// unchecked (the spec-writer spec's Design, "The two writes R1 leaves as they are").
const anchoredWriteExemption = "an anchored comment, ask or suggestion writes a version (MarkQuote and SnapshotVersion, anchors.go) " +
	"that adds only its writer's mark around words others wrote, and a comment or a suggestion is how a worker " +
	"proposes a change to the architect; dispatch suggest posts on the two comment routes " +
	"(packages/envoy-client/src/dispatch-commands/conversation.ts)"

// premortemConvertExemption is why specWriterExempt leaves the pre-mortem conversion unchecked.
const premortemConvertExemption = "the conversion's one pointer is the moved_to out of the pre-mortem document it retires, to " +
	"the lens it writes (retireConvertedDocument), a relation out of a document and not out of a requirement, criterion " +
	"or check node; the lens and the risks it writes are none of those nodes, and a risk's prevented_by at a check is a " +
	"relation out of the risk"

// specWriterExempt names every route that reaches a document write, or a node write of
// specWriterNodeSinks, and is left unchecked, with the reason it is left. A route here is held to
// two things (TestTheSpecWriterExemptionsStillHold): the table serves it, so a renamed row leaves
// no reason pointing at nothing, and its path still does not call the check, so an exemption that
// has stopped being true is deleted rather than left to describe the opposite of what the handler
// does. No test hook reaches a document write today: a hook that does goes in with the reason "a
// test hook, mounted only with TestHooksEnabled".
var specWriterExempt = map[string]string{
	"POST /api/v1/issues/{key}/comments":                           anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/comments":                         anchoredWriteExemption,
	"POST /api/v1/issues/{key}/asks":                               anchoredWriteExemption,
	"POST /api/v1/artifacts/{id}/asks":                             anchoredWriteExemption,
	"POST /api/v1/issues/{key}/artifacts/{slug}/premortem-convert": premortemConvertExemption,
}

// TestEveryRouteThatWritesADocumentChecksItsWriter holds R1 of the spec-writer spec - on a
// Legion-run issue, a version of the spec or the Verification document from a writer who is no
// person and no architect is refused on every route that writes one - to the server's own route
// inventory rather than to a list of the routes that check today; and with it the rule that a write
// of a requirement, criterion or check node's state, or of a pointer out of one, is held to the
// same check, so the guard counts the node writes of specWriterNodeSinks beside the sinks.
//
// The inventory is the table (routes_table.go) with its test hooks in, read from the source so that
// each row's handler is read with it, and held to the table the server mounts. A row whose path
// reaches a sink (specWriterSinks, specWriterNodeSinks) passes when its path also calls the check
// (specWriterCheck), when its row declares the person scope in routeScopes, which no worker's
// credential holds, or when specWriterExempt names it with its reason. Its failure names each route
// and each sink it reaches, with the calls that reach it.
//
// The same walk finds each route whose path reaches the query revision (specWriterRevision), which
// is no sink, and the test holds those routes to specWriterRevisions: a route that reaches it and
// is not named there fails, naming it and the calls that reach it, and so does a route named there
// that the table does not serve or whose path no longer reaches it (unheldQueryRevisions).
//
// The walk is decisionBlockChecked's: every call of every function on the path inside package api,
// followed as the copy-lock graph resolves it, plus every function handed to it as an argument. It
// reads each body whole, branches included, so a route reaches every sink of every function on its
// path, and a handler that calls the check under a condition passes here as it does in the other
// static guards of this package; the behavior tests (spec_writer_test.go, node_writer_test.go) hold
// the branches.
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
	}
	for _, sink := range specWriterNodeSinks {
		if graph.named[sink] == nil {
			t.Errorf("%s, a node write the guard looks for, is gone: point specWriterNodeSinks at its replacement", sink)
		}
	}
	for _, sink := range slices.Concat(specWriterSinks, specWriterNodeSinks) {
		if !slices.ContainsFunc(slices.Collect(maps.Values(routes)), func(reach specWriterReach) bool { return len(reach.sinks[sink]) > 0 }) {
			t.Errorf("no route of the table reaches %s, so the guard sees none of its calls", sink)
		}
	}
	if graph.named["docs.Service."+specWriterRevision] == nil {
		t.Errorf("docs.Service.%s, the query revision specWriterRevisions names the routes of, is gone: "+
			"point specWriterRevision at its replacement", specWriterRevision)
	}

	for _, name := range slices.Sorted(maps.Keys(routes)) {
		reach := routes[name]
		if reason, listed := specWriterRevisions[name]; listed && len(reach.revises) > 0 {
			t.Logf("%s: reaches %s (%s); a revision route - %s", name, specWriterRevision, strings.Join(reach.revises, ", "), reason)
		}
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
		t.Errorf("a route writes a document or a node and never checks its writer:\n\t%s\n"+
			"Call refuseSpecWriter on the route's path before its first document call or node write (spec_writer.go), "+
			"declare model.ScopePerson for the route in routeScopes, or name it in specWriterExempt with the reason it is left.",
			strings.Join(unchecked, "\n\t"))
	}
	if unheld := unheldQueryRevisions(routes, specWriterRevisions); len(unheld) > 0 {
		t.Errorf("the routes that reach %s are not the ones specWriterRevisions names:\n\t%s\n"+
			"Name a route that reaches it in specWriterRevisions with the reason its version writes no requirement, criterion "+
			"or check node and no value or relation out of one, and delete a line whose route no longer reaches it, or point it "+
			"at the row that replaced it.",
			specWriterRevision, strings.Join(unheld, "\n\t"))
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
// helper names any of them. A third control holds the revision list the same way: over a planted
// list naming one route that reaches DrainOwedQueries and one that reaches nothing, the decision
// names the planted route that reaches DrainOwedQueries unnamed and the named route that does not reach it,
// and neither the named revision route nor any route of the first two controls.
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
	revisions := map[string]string{
		"POST /planted/revised": "a planted revision route",
		"GET /planted/read":     "a planted revision route whose path reaches no revision",
	}
	unheld := unheldQueryRevisions(routes, revisions)
	if len(unheld) != 2 || !strings.HasPrefix(unheld[0], "POST /planted/revised-unnamed: reaches "+specWriterRevision+" (") ||
		!strings.HasPrefix(unheld[1], "GET /planted/read: its path no longer reaches "+specWriterRevision) {
		t.Errorf("the revision list's decision names %q over the planted table, want the route that reaches %[2]s unnamed, "+
			"POST /planted/revised-unnamed, then the named route whose path does not reach %[2]s, GET /planted/read", unheld, specWriterRevision)
	}
}

// specWriterReach is what one route's path runs: each sink it reaches, by method name, with every
// call on the path that reaches it ("<file>:<line>", sorted); every call on the path that reaches
// the query revision (specWriterRevision), sorted; and whether it calls the check.
type specWriterReach struct {
	sinks   map[string][]string
	revises []string
	checks  bool
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

// unheldQueryRevisions is the revision list's decision over routes: each route whose path reaches
// the query revision (specWriterRevision) and that revisions does not name, with the calls that
// reach it, then each route revisions names that routes does not hold, or whose path no longer
// reaches the revision, with the reason it was named.
// TestEveryRouteThatWritesADocumentChecksItsWriter runs it over the server's table and
// TestTheSpecWriterGuardSeesAPlantedWriter over a planted one.
func unheldQueryRevisions(routes map[string]specWriterReach, revisions map[string]string) []string {
	var unheld []string
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		if _, named := revisions[name]; !named && len(routes[name].revises) > 0 {
			unheld = append(unheld, fmt.Sprintf("%s: reaches %s (%s), and specWriterRevisions does not name it",
				name, specWriterRevision, strings.Join(routes[name].revises, ", ")))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(revisions)) {
		reach, served := routes[name]
		switch {
		case !served:
			unheld = append(unheld, name+": the table serves no such route")
		case len(reach.revises) == 0:
			unheld = append(unheld, fmt.Sprintf("%s: its path no longer reaches %s, so its reason (%s) describes nothing it does",
				name, specWriterRevision, revisions[name]))
		}
	}
	return unheld
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

// specWriterStep is what one function's own body runs: the sinks it calls, by method name for a
// sink of the document service and by the graph's name for a node write (specWriterNodeSinks),
// with where; where it calls the query revision; whether it calls the check; and the functions of
// package api its calls run.
type specWriterStep struct {
	sinks   map[string][]string
	revises []string
	checks  bool
	next    []*lockFunction
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
		for _, call := range step.revises {
			if !slices.Contains(reach.revises, call) {
				reach.revises = append(reach.revises, call)
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
	slices.Sort(reach.revises)
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
		if slices.Contains(specWriterNodeSinks, name) {
			step.sinks[name] = append(step.sinks[name], w.graph.line(fn.source, call))
		}
		if method, called := specWriterServiceMethod(name); called {
			switch {
			case slices.Contains(specWriterSinks, method):
				step.sinks[method] = append(step.sinks[method], w.graph.line(fn.source, call))
			case method == specWriterRevision:
				step.revises = append(step.revises, w.graph.line(fn.source, call))
			}
		}
		for _, callee := range w.graph.calleesAndPassed(call) {
			if callee != nil && callee.pkg == specWriterGuardPkg && !slices.Contains(step.next, callee) {
				step.next = append(step.next, callee)
			}
		}
	}
	return step
}

// specWriterServiceMethod is the method of the document service name names, as
// calledFunctionName gives a call's function, called through either of specWriterSinkTypes, and
// whether it names one: a sink where specWriterSinks holds it, the query revision where it is
// specWriterRevision.
func specWriterServiceMethod(name string) (string, bool) {
	for _, through := range specWriterSinkTypes {
		if method, called := strings.CutPrefix(name, through); called {
			return method, true
		}
	}
	return "", false
}

// plantedSpecWriterSources is the planted module TestTheSpecWriterGuardSeesAPlantedWriter reads, by
// import path: a document service with ApplyOps and DrainOwedQueries behind an interface, as docs.API
// holds them; the stand-ins for net/http's method names the table's rows spell; and a server whose
// routes table holds a route that writes through a function literal with no check, a route whose
// path calls the check through a helper before the same write, a route that needs the person scope
// and writes the same way, an exempt route that checks its writer all the same, a route that writes
// nothing, and two routes that revise an issue's queries through a helper and write nothing else.
// Each package's name is the one the guard reads (docs, http, api), so the graph names its
// functions as it names the server's.
var plantedSpecWriterSources = map[string]string{
	"planted/docs": `package docs

type API interface {
	ApplyOps(artifactID string) error
	DrainOwedQueries(issueKey string) error
}

type Service struct{}

func (s *Service) ApplyOps(artifactID string) error { return nil }

func (s *Service) DrainOwedQueries(issueKey string) error { return nil }
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
		{http.MethodPost, "/planted/revised", "any", "Revises the issue's queries, and is named.", s.revise},
		{http.MethodPost, "/planted/revised-unnamed", "any", "Revises the issue's queries, and is not named.", s.revise},
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

func (s *server) revise() {
	s.inTransaction(func() error { return s.documents.DrainOwedQueries("KEY-1") })
}
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
