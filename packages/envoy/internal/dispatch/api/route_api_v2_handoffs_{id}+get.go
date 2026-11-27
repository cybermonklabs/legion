package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/handoffs/{id}", Auth: authAny, Versions: onlyV2,
		Description: "One handoff by id with its body, the exact text written: {id, issue_key, phase, version, author, " +
			"created_at, body}; a closed issue's too. A caller that sent Dispatch-Trees: 1 is also given field_trees: " +
			"the tree a reader is shown of each string the body's card draws, keyed by that string's JSON Pointer into " +
			"the body (/summary, /proof/0), as GET /api/v1/handoffs/{id} gives it. 404 HANDOFF_NOT_FOUND for an id no " +
			"handoff has.",
		Handler:  s.getHandoffV2,
		Resource: "handoffs", Kind: kindItem, Key: "{id}",
		Shape:    routeShape{Status: http.StatusOK, Response: handoffRead{}},
		IDs:      &idRoute{Reason: "a handoff id: one phase's record on an issue"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, handoffMissingRefusal},
	}
})
