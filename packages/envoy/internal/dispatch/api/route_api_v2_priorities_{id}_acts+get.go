package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/priorities/{id}/acts", Auth: authAny, Versions: onlyV2,
		Description: "One priority's own acts: every act stated about it as {field, value, issue, asserted, actor, at}. " +
			"field is what was stated - title, description, owner and rank, the four a person writes, state (live when " +
			"it was made, gone when it was deleted) and visibility; advances is the relation an issue states on it. " +
			"value is what an attribute write wrote and is empty on an advances act; issue is the issue at the other end " +
			"of an advances act and is empty on an attribute write; asserted is false on the act that retracted an " +
			"advances and true on every other. sort=-at, the default, lists the newest first, and sort=at the oldest. A " +
			"deleted priority still answers its acts, the delete its newest. limit 1 to 200 (default 50) and offset; " +
			"{items, total, limit, offset}. 404 PRIORITY_NOT_FOUND for an id no priority carries.",
		Handler:  s.listPriorityActsV2,
		Resource: "acts", Kind: kindCollection,
		Query:    priorityActsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.PriorityAct]{}},
		IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
		Refusals: listRefusals(priorityMissingRefusal),
	}
})
