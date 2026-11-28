package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/answers", Auth: authAny, Versions: onlyV2,
		Description: "The caller's own answers and replies on asks across issues and project documents, newest " +
			"first: each item is an answer (an ask.answered event's answer, and whether it is still the ask's current " +
			"one) or a reply, {kind, at, ask_id, ref, question, ask_kind, ask_state, edited_at, owner, answer?, " +
			"current, reply?}, with the ask's question, state, owner and deep link. Needs the person scope. limit 1 to " +
			"200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listMyAnswersV2,
		Resource: "answers", Kind: kindCollection,
		Query:       myAnswersV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[model.MyAnswerRow]{}},
		List:        listBounds{Default: myAnswersDefaultLimit, Max: myAnswersMaxLimit},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
