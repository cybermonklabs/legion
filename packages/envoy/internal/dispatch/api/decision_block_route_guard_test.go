//go:build !race

// The guard is static, as the copy-lock and keyed-write guards whose graph it reads are: it reads
// the module's source, so the race detector adds nothing to it, and leaving it out of the -race
// run spares that job type-checking the server a second time.

package api

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// sharedBodyCheck is the one check a stored body is held to (itemblocks.Check): the bounds on
// its blocks, and the decision block it may not hold. sharedBodyRefusal is the shared helper
// that words that check's answer (blockBoundError, server.go), which is what makes
// DECISION_BLOCK_IN_BODY one refusal with one wording rather than one per route. Both are
// written as the call graph names a function: its package's name, then its own.
const (
	sharedBodyCheck       = "itemblocks.Check"
	sharedBodyRefusal     = "api.blockBoundError"
	decisionBlockGuardPkg = "api"
)

// decisionBlockExempt names every POST or PATCH route whose request carries a person's prose that
// this rule leaves unrefused, with the reason it is left. A route here is held to two things
// below: it is a route the server serves, so a renamed row cannot leave a reason behind pointing
// at nothing, and it still runs no check, so an exemption that has stopped being true is deleted
// rather than left to describe the opposite of what the handler does.
//
// Everything that takes a `body` string is held to the check itself
// (TestEveryRouteThatTakesABodyRefusesADecisionBlock), so a writer added later is held to it
// without being named anywhere. A route whose person-facing prose rides in a member of its own
// is read by no walk, so this map is a hand-kept list there: it names the routes known to carry
// such prose unrefused, and nothing derives that list or fails when a later route joins it.
var decisionBlockExempt = map[string]string{
	"POST /api/v1/issues/{key}/handoffs/{phase}": "the body is one JSON object, held to contracts.DispatchHandoffBodyMax and stored exactly as sent (handoffs.go); every reader is given its fields as JSON and none of it as Markdown, so no decision card is ever drawn from it",

	// The five ask writes. The reason is one decision, recorded as "An ask's own question, its
	// options and its answers are left alone" (dispatch://INFRA-1201/artifact/decisions-md): the
	// same card is drawn for a decision block written in an ask's question, in an option's label,
	// in an answer's text or in a resolution's reason, and none of those writes is refused,
	// because an ask already reaches a person, so a block inside one is ugly rather than silent;
	// because that wording is held by the ask's own check (validateAskWording, asks.go) and not
	// by the body bound this rule sits beside; and because refusing a person's answer text would
	// stop the one act this issue exists to protect.
	"POST /api/v1/issues/{key}/asks":      "an ask's question and options are held by validateAskWording, and a decision block in an ask is left alone (dispatch://INFRA-1201/artifact/decisions-md)",
	"POST /api/v1/artifacts/{id}/asks":    "an ask's question and options are held by validateAskWording, and a decision block in an ask is left alone (dispatch://INFRA-1201/artifact/decisions-md)",
	"PATCH /api/v1/asks/{id}":             "an edit's question and options are held by validateAskWording against the ask as it will be stored, and a decision block in an ask is left alone (dispatch://INFRA-1201/artifact/decisions-md)",
	"POST /api/v1/asks/{id}/answer":       "refusing a person's answer text would stop the one act this issue exists to protect; the text is held to contracts.DispatchEventBodyMax (ask_transitions.go)",
	"POST /api/v1/asks/{id}/resolve":      "a resolution's reason is a person's or a session's own wording on an ask that already reached a person, left alone by the same decision (dispatch://INFRA-1201/artifact/decisions-md)",
	"POST /api/v1/artifacts/{id}/reviews": "while an approval request is open this route answers it, closing the ask with answerTransition, and the decisions document leaves an answer's text alone (dispatch://INFRA-1201/artifact/decisions-md); the same reason text reaches artifact_reviews unchecked through POST /api/v1/asks/{id}/answer (ask_transitions.go), so refusing one side would be inconsistent rather than protective. A decision block written there still draws a card nobody can answer on the approval chip, which this issue does not close",
}

// decisionBlockGuardSees are the three writes the issue exists for, named so the guard cannot
// pass by seeing nothing: a comment, a message and a broadcast. They are a control over the walk,
// never the rule — the rule is read from the table and the shapes, and holds every route they
// name as well as these.
var decisionBlockGuardSees = []string{
	"POST /api/v1/issues/{key}/comments",
	"POST /api/v1/issues/{key}/messages",
	"POST /api/v1/broadcasts",
}

// TestEveryRouteThatTakesABodyRefusesADecisionBlock holds R2 of INFRA-1201 — the rule is written
// once, and a writer added later gets it — to the server's own route inventory rather than to a
// list of the paths that have it today.
//
// The inventory is the table (routes_table.go) with its test hooks in, read from the source so
// that each row's handler is read with it, joined to the Go type each row takes (routeShapes,
// routes_shapes.go). A row is in scope when its method is one register keys (keyedMethod: a POST
// or a PATCH) and its request type carries a `body` string, the field every stored comment,
// message, broadcast and handoff body is written into. A row in scope either runs the shared
// check — somewhere in what its handler runs inside package api, a call of blockBoundError over a
// call of itemblocks.Check, which is the one place DECISION_BLOCK_IN_BODY is worded — or stands
// in decisionBlockExempt with the reason it does not. A row the shapes do not describe is named
// too: without a request type the guard cannot tell whether it takes a body, and a guard that
// cannot tell must not pass.
//
// The walk stops at the edge of package api, because blockBoundError is unexported and every path
// to it is inside this package; a function literal a handler hands to another package is still
// walked, since the call that hands it over is read here. What it proves is that the check stands
// on the route's path, not that it is reached on every branch of it: a handler that calls it under
// a condition passes here, as it does in the other static guards of this package
// (keyed_write_guard_test.go), and the route's own tests hold the branch.
func TestEveryRouteThatTakesABodyRefusesADecisionBlock(t *testing.T) {
	graph := copyLockGraph(t)
	for _, function := range []string{sharedBodyCheck, sharedBodyRefusal} {
		if graph.named[function] == nil {
			t.Fatalf("%s, the check this guard reads a body route by, is gone: point the guard at its replacement", function)
		}
	}
	routes := decisionBlockRouteHandlers(t, graph)

	var unchecked, unshaped, stale []string
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		checks := decisionBlockChecked(graph, routes[name])
		reason, exempt := decisionBlockExempt[name]
		shape, shaped := routeShapes[name]
		switch {
		case exempt:
			t.Logf("%s: exempt — %s", name, reason)
			if checks {
				stale = append(stale, fmt.Sprintf("%s: runs the check now, so its exemption (%s) is no longer true", name, reason))
			}
		case !shaped:
			unshaped = append(unshaped, name+": no shape (routeShapes), so whether it takes a body cannot be read")
		case !requestCarriesBody(shape.Request):
			t.Logf("%s: takes no body string", name)
		case checks:
			t.Logf("%s: checked", name)
		default:
			unchecked = append(unchecked, fmt.Sprintf("%s: takes a body and never reaches %s(%s(...))", name, sharedBodyRefusal, sharedBodyCheck))
		}
	}
	if len(unchecked) > 0 {
		t.Errorf("a body a person reads is stored by a route that refuses no decision block in it:\n\t%s\n"+
			"Call blockBoundError(itemblocks.Check(\"body\", <the body>)) in the handler, as the comment, message and broadcast writes do, or name the route in decisionBlockExempt with the reason it is left alone.",
			strings.Join(unchecked, "\n\t"))
	}
	if len(unshaped) > 0 {
		t.Errorf("a POST or PATCH route has no request type to read:\n\t%s\nGive it one in routeShapes, which the OpenAPI document is reflected out of.", strings.Join(unshaped, "\n\t"))
	}
	if len(stale) > 0 {
		t.Errorf("an exemption describes the opposite of what its route does:\n\t%s\nDelete the route's line from decisionBlockExempt.", strings.Join(stale, "\n\t"))
	}
	if unmounted := routesNotMounted(maps.Keys(decisionBlockExempt)); len(unmounted) > 0 {
		t.Errorf("decisionBlockExempt names routes the server does not serve: %s\nDelete each line, or point it at the row that replaced it.", strings.Join(unmounted, ", "))
	}
}

// TestTheDecisionBlockGuardSeesWhatItGuards holds the guard to what it must see, so that a walk
// that found nothing — a table it could not read, a handler it could not follow — could not pass
// as a server that checks everything: the three writes the issue exists for are in its inventory,
// each taking a body and each reaching the check.
func TestTheDecisionBlockGuardSeesWhatItGuards(t *testing.T) {
	graph := copyLockGraph(t)
	routes := decisionBlockRouteHandlers(t, graph)
	for _, name := range decisionBlockGuardSees {
		handlers, mounted := routes[name]
		if !mounted {
			t.Errorf("the guard reads no %s off the table", name)
			continue
		}
		shape, shaped := routeShapes[name]
		if !shaped || !requestCarriesBody(shape.Request) {
			t.Errorf("the guard reads no body string on %s, the route that stores one", name)
		}
		if !decisionBlockChecked(graph, handlers) {
			t.Errorf("the guard does not reach %s(%s(...)) from %s, which calls it", sharedBodyRefusal, sharedBodyCheck, name)
		}
	}
}

// TestTheGuardReadsABodyStringOffARequestType pins what puts a route in scope, which is the part
// of the guard that can go blind without a test failing anywhere: a request type whose body field
// is renamed, or whose body stops being a string, drops its route out of the inventory and takes
// the rule off it in silence.
func TestTheGuardReadsABodyStringOffARequestType(t *testing.T) {
	type embedded struct {
		Body string `json:"body"`
	}
	for _, probe := range []struct {
		name    string
		request any
		carries bool
	}{
		{"a route that takes no body at all", nil, false},
		{"the comment write", commentInput{}, true},
		{"the comment edit", editCommentInput{}, true},
		{"the broadcast write", createBroadcastInput{}, true},
		{"a body sent as a pointer", struct {
			Body *string `json:"body"`
		}{}, true},
		{"a body promoted from an embedded struct", struct{ embedded }{}, true},
		{"a body named under tag options", struct {
			Body string `json:"body,omitempty"`
		}{}, true},
		{"a field named body in Go and something else in JSON", struct {
			Body string `json:"reason"`
		}{}, false},
		{"a body that is no string", struct {
			Body []string `json:"body"`
		}{}, false},
		{"an ask write, whose prose is its question", createAskInput{}, false},
	} {
		if carries := requestCarriesBody(probe.request); carries != probe.carries {
			t.Errorf("%s: the guard reads a body string = %t, want %t", probe.name, carries, probe.carries)
		}
	}
}

// decisionBlockRouteHandlers is every POST and PATCH row of the table, by "<method> <pattern>",
// with the handler bodies that row mounts (writeRouteHandlers).
func decisionBlockRouteHandlers(t *testing.T, graph *lockGraph) map[string][]*lockFunction {
	t.Helper()
	return writeRouteHandlers(t, graph, keyedMethod)
}

// writeRouteHandlers is every row of the table whose method include takes, by "<method>
// <pattern>", with the handler bodies that row mounts. It is read from the source of routes()
// rather than from a server's own table so that each row's handler comes with it: a handler read
// at run time is a function value, and what a guard asks is what that function's source runs.
func writeRouteHandlers(t *testing.T, graph *lockGraph, include func(method string) bool) map[string][]*lockFunction {
	t.Helper()
	table := graph.named["api.server.routes"]
	if table == nil {
		t.Fatal("the guard finds no routes table (api.server.routes)")
	}
	routes := map[string][]*lockFunction{}
	ast.Inspect(table.body, func(node ast.Node) bool {
		row, ok := node.(*ast.CompositeLit)
		if !ok || len(row.Elts) != 5 {
			return true
		}
		method, ok := ast.Unparen(row.Elts[0]).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		verb := strings.ToUpper(strings.TrimPrefix(method.Sel.Name, "Method"))
		if !include(verb) {
			return false
		}
		pattern, ok := literalText(row.Elts[1], table.pkg, graph.constants)
		if !ok {
			t.Errorf("the guard cannot read the pattern of a %s row of the table", verb)
			return false
		}
		routes[verb+" "+pattern] = graph.functionValue(row.Elts[4])
		return false
	})
	if len(routes) == 0 {
		t.Fatal("the guard reads no row of the methods it asks for off the routes table")
	}
	return routes
}

// decisionBlockChecked reports whether running one of handlers runs the shared check: a call of
// blockBoundError over a call of itemblocks.Check, anywhere in what the handler runs inside
// package api (routeReaches).
func decisionBlockChecked(graph *lockGraph, handlers []*lockFunction) bool {
	return routeReaches(graph, handlers, func(call *ast.CallExpr) bool { return checksABody(graph, call) })
}

// routeReaches reports whether running one of handlers makes a call matches takes, anywhere in
// what the handler runs inside package api. A call is followed as the copy-lock graph resolves
// it, plus every function handed to it as an argument, whatever the callee does with it; a
// function of another package is not walked, since the helpers the guards look for are
// unexported and no path to them leaves this one.
func routeReaches(graph *lockGraph, handlers []*lockFunction, matches func(*ast.CallExpr) bool) bool {
	walked := map[*lockFunction]bool{}
	var runs func(fn *lockFunction) bool
	runs = func(fn *lockFunction) bool {
		if fn == nil || fn.pkg != decisionBlockGuardPkg || walked[fn] {
			return false
		}
		walked[fn] = true
		for _, call := range ownCalls(fn) {
			if matches(call) {
				return true
			}
			for _, callee := range graph.callees(call) {
				if runs(callee) {
					return true
				}
			}
			for _, argument := range call.Args {
				for _, passed := range graph.functionValue(argument) {
					if runs(passed) {
						return true
					}
				}
			}
		}
		return false
	}
	for _, handler := range handlers {
		if runs(handler) {
			return true
		}
	}
	return false
}

// checksABody reports whether call is the shared check worded by the shared helper:
// blockBoundError over a call of itemblocks.Check, which is how all six callers write it today.
// The check is read inside the helper's arguments rather than beside it, so that a route holding
// its answer in a variable first is read the same way, and so that a call of either one without
// the other — a body checked and refused in words of its own, or the helper handed an error no
// check made — is no check of a body.
func checksABody(graph *lockGraph, call *ast.CallExpr) bool {
	if calledFunctionName(graph, call) != sharedBodyRefusal {
		return false
	}
	checked := false
	for _, argument := range call.Args {
		ast.Inspect(argument, func(node ast.Node) bool {
			inner, ok := node.(*ast.CallExpr)
			if !ok || calledFunctionName(graph, inner) != sharedBodyCheck {
				return !checked
			}
			checked = true
			return false
		})
	}
	return checked
}

// calledFunctionName is the name of the function or method call names, as the graph names one
// (functionName); it is empty for a call through a value, a conversion or anything else the type
// information does not resolve to a function.
func calledFunctionName(graph *lockGraph, call *ast.CallExpr) string {
	var object types.Object
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		object = graph.info.Uses[fun]
	case *ast.SelectorExpr:
		if selection, ok := graph.info.Selections[fun]; ok {
			object = selection.Obj()
		} else {
			object = graph.info.Uses[fun.Sel]
		}
	}
	fn, ok := object.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return ""
	}
	return functionName(fn)
}

// requestCarriesBody reports whether request, the zero value of a route's request type
// (routeShape.Request), carries a `body` string: the JSON member every stored comment, message,
// broadcast and handoff body is written into. A pointer is read through, since a body a route
// takes as optional is a *string, and an embedded struct is read through too, since a member it
// promotes is a member of the request body.
func requestCarriesBody(request any) bool {
	return carriesBodyString(reflect.TypeOf(request))
}

func carriesBodyString(request reflect.Type) bool {
	for request != nil && request.Kind() == reflect.Pointer {
		request = request.Elem()
	}
	if request == nil || request.Kind() != reflect.Struct {
		return false
	}
	for index := range request.NumField() {
		field := request.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Anonymous && name == "" {
			if carriesBodyString(field.Type) {
				return true
			}
			continue
		}
		if name != "body" {
			continue
		}
		member := field.Type
		for member.Kind() == reflect.Pointer {
			member = member.Elem()
		}
		if member.Kind() == reflect.String {
			return true
		}
	}
	return false
}
