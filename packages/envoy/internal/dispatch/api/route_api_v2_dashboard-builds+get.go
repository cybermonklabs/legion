package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/dashboard-builds", Auth: authAny, Versions: onlyV2,
		Description: "Which dashboard builds are served: each {entry, serving}, entry the entry chunk " +
			"(/assets/index-*.js) of a build some live server process serves, sorted, and serving whether it is the " +
			"one this process serves. A tab offers its reload notice only when total is 1 and that entry is not the " +
			"tab's own: while a rolling update or a rollback runs two builds, each tab keeps the page it has. limit 1 " +
			"to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listDashboardBuildsV2,
		Resource: "dashboard-builds", Kind: kindCollection,
		Query:    dashboardBuildsV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[dashboardBuildV2]{}},
		Refusals: listRefusals(),
	}
})
