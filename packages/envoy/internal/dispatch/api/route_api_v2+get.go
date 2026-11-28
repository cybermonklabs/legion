package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "", Auth: authPublic, Versions: onlyV2,
		Description: "This index: every /api/v2 route with its method, path, auth, scopes and purpose, and what a client reads of it: the resource it serves and that resource's kind and key, the time fields a read windows on, the field a log is read in order by, the bounds of a list's limit, and whether it reads across projects. {docs, items, total, limit, offset}, sorted by path then method; every route is on one page unless the request names a limit (any of 1 or more) or an offset.",
		Handler:     s.getRouteIndexV2,
		Resource:    "routes",
		Kind:        kindCollection,
		List:        listBounds{Whole: true},
		Query:       routeIndexV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: routeIndexV2{}},
		Refusals:    listRefusals(),
	}
})
