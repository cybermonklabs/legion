//go:build !race

// The guard is static, as the decision-block guard whose call-graph walk it shares: it reads the
// module's source, so the race detector adds nothing to it.

package api

import (
	"go/ast"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestTheRegistryRoutesThatCarryProse lists the routes the writing check reads, so a reader of the
// test log sees them, and holds the list to the five ask routes the decision-block rule leaves
// alone: the writing check reads every one of them.
func TestTheRegistryRoutesThatCarryProse(t *testing.T) {
	for _, name := range []string{
		"POST /api/v1/issues/{key}/asks", "POST /api/v1/artifacts/{id}/asks", "PATCH /api/v1/asks/{id}",
		"POST /api/v1/asks/{id}/answer", "POST /api/v1/asks/{id}/resolve",
	} {
		if !requestCarriesProse(routeShapes[name].Request) {
			t.Errorf("%s carries no prose, want the ask's own words read", name)
		}
		if _, exempt := decisionBlockExempt[name]; !exempt {
			t.Errorf("%s is no longer in decisionBlockExempt; this change leaves that rule alone", name)
		}
	}
	for _, name := range []string{"POST /api/v1/issues/{key}/handoffs/{phase}", "POST /api/v1/projects/{key}/related", "POST /api/v1/going/requests"} {
		if requestCarriesProse(routeShapes[name].Request) {
			t.Errorf("%s carries prose, want none: it is no prose a person reads", name)
		}
	}
}

// --- the call-graph guard --------------------------------------------------------------------

// writingCheckFunction is the check every prose route calls, as the call graph names it.
const writingCheckFunction = "api.server.writingCheck"

// writingCheckMethods are the methods a write takes a body on.
func writingCheckMethods(method string) bool {
	return method == http.MethodPost || method == http.MethodPatch || method == http.MethodPut
}

func reachesWritingCheck(graph *lockGraph, handlers []*lockFunction) bool {
	return routeReaches(graph, handlers, func(call *ast.CallExpr) bool {
		return calledFunctionName(graph, call) == writingCheckFunction
	})
}

// TestEveryRouteThatTakesProseReachesTheWritingCheck holds R1 of INFRA-3231 to the server's own
// route inventory, as the decision-block guard holds its rule: every POST, PATCH or PUT row of the
// table whose request type carries a member the registry marks prose reaches writingCheck in what
// its handler runs inside package api. A row the shapes do not describe is named too.
func TestEveryRouteThatTakesProseReachesTheWritingCheck(t *testing.T) {
	graph := copyLockGraph(t)
	if graph.named[writingCheckFunction] == nil {
		t.Fatalf("%s, the check this guard looks for, is gone: point the guard at its replacement", writingCheckFunction)
	}
	routes := writeRouteHandlers(t, graph, writingCheckMethods)
	var unchecked, unshaped []string
	checked := 0
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		shape, shaped := routeShapes[name]
		switch {
		case !shaped:
			unshaped = append(unshaped, name)
		case !requestCarriesProse(shape.Request):
		case reachesWritingCheck(graph, routes[name]):
			checked++
			t.Logf("%s: checked", name)
		default:
			unchecked = append(unchecked, name)
		}
	}
	if len(unchecked) > 0 {
		t.Errorf("a route whose request carries prose never reaches %s:\n\t%s\n"+
			"Call s.writingCheck(w, r, actor, input) once the input is decoded and the actor resolved, before any transaction.",
			writingCheckFunction, strings.Join(unchecked, "\n\t"))
	}
	if len(unshaped) > 0 {
		t.Errorf("a write route has no request type to read: %s", strings.Join(unshaped, ", "))
	}
	if checked < 25 {
		t.Errorf("the guard saw %d checked prose routes, want every prose route of the table", checked)
	}
}

// TestTheWritingCheckGuardFailsAnUncheckedProseRoute is the guard's negative control: a write
// route whose handler never calls the check is read as unchecked. The handoff write takes a body
// the registry marks not prose and never calls the check, so a prose route written as it is
// written would fail the guard; the comment write calls it, and is read as checked.
func TestTheWritingCheckGuardFailsAnUncheckedProseRoute(t *testing.T) {
	graph := copyLockGraph(t)
	routes := writeRouteHandlers(t, graph, writingCheckMethods)
	if reachesWritingCheck(graph, routes["POST /api/v1/issues/{key}/handoffs/{phase}"]) {
		t.Error("the handoff write reaches the writing check, want a handler that does not as the control")
	}
	if !reachesWritingCheck(graph, routes["POST /api/v1/issues/{key}/comments"]) {
		t.Error("the comment write does not reach the writing check, which it calls")
	}
	if routeReaches(graph, routes["POST /api/v1/issues/{key}/comments"], func(call *ast.CallExpr) bool {
		return calledFunctionName(graph, call) == "api.server.noSuchCheck"
	}) {
		t.Error("the walk reached a function no route calls")
	}
}
