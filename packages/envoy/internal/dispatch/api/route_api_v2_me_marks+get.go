package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/marks", Auth: authAny, Versions: onlyV2,
		Description: "The caller's stored marks, of the kind kind= names (issue, agent, document, thread, message " +
			"or ask; every kind where it names none), each of the ids id= names (repeatable; every id where it names " +
			"none), by kind in that order and then by id. " + markText + ". An agent mark is listed for every session " +
			"the caller has cleared, read, pinned or holds an unread reply from; a thread mark for every thread the " +
			"caller wrote in; a mark of any other kind once the caller has written it. Needs the person scope. limit 1 " +
			"to 200 (default 50) and offset; {items, total, " +
			"limit, offset}.",
		Handler:  s.listMarksV2,
		Resource: "marks", Kind: kindCollection, Key: "{kind}/{id}",
		Query:       markListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[markV2]{}},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(),
	}
})
