package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/ci-reconcile-passes", Auth: authAny, Versions: onlyV2,
		Description: "The CI reconcile passes that started within from and to, RFC3339, from inclusive and to " +
			"exclusive, newest first; from defaults to 24 hours before to, or before now where to is absent, and an " +
			"absent to sets no end, so a request naming neither reads every pass from 24 hours ago on, and a from not " +
			"before to is 400 INVALID_QUERY. Rows are kept for 48 " +
			"hours and pruned by the pass that ends. Each pass is its id, the server process that ran it (instance), " +
			"started_at and ended_at, what it did (counts: graphql_queries, graphql_points, rest_requests, " +
			"rest_not_modified, heads_over_rest, records, heads, heads_known) and where it left the cursor, which is " +
			"where the pass after it takes the order up (cursor: repository, from, number, rest, counted); both are " +
			"null until the pass ends. state is running, complete, deadline (its own five-minute bound cut it " +
			"short), rate_limit (a shared GitHub limit ended it), cancelled (its process shut down under it) or " +
			"ended_without_completing, a pass whose process died before it could record its end: a pass with no end " +
			"reads running only while its process is live, the lease is held and it is the newest pass of all, " +
			"inside the window or not. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listCIReconcilePassesV2,
		Resource: "ci-reconcile-passes", Kind: kindCollection, TimeFields: []string{"started_at"},
		Query:    ciReconcilePassesV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[cireconcile.PassRecord]{}},
		Refusals: listRefusals(),
	}
})
