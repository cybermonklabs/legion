package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/going-actions", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("The catalogue of operations an agent may propose on Dispatch's graph, at their /api/v2 "+
			"routes: %d of the %d tool specs agents operate Dispatch with, every one that writes but dispatch_review "+
			"and dispatch_project_update, "+
			"each carried whole - its own name (action), its own description, the arguments it takes (request), each "+
			"with the bounds its spec states, min and max (a text's length in UTF-16 units, a list's count of items or a "+
			"number's value, both left out where it states none), with "+
			"an example call, the /api/v2 rows it writes in the order a call is matched against them (routes, each "+
			"{method, pattern, body?}), and the graph relations its verbs write. A route's body is the fixed part of "+
			"the body a call of it sends beside the action's arguments, such as {\"state\": \"resolved\"} on PUT "+
			"/api/v2/asks/{id}/state; an argument that picks between two routes of one action (dispatch_claim's "+
			"release, dispatch_follow's action, dispatch_resolve_ask's kind) is sent to neither. It leaves the two "+
			"out on purpose: a review records an approval or a changes request in a person's name, "+
			"dispatch_project_update turns a check of a project on or off, and the loop that proposes operations "+
			"must never propose either. Every action is on one page unless the "+
			"request names a limit (any of 1 or more) and an offset; {items, total, limit, offset}.",
			len(goingToolRoutesV2), len(contracts.DispatchTools)),
		Handler:  s.listGoingActionsV2,
		Resource: "going-actions", Kind: kindCollection,
		Query:    goingActionsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[goingActionV2]{}},
		List:     listBounds{Whole: true},
		Refusals: listRefusals(),
	}
})
