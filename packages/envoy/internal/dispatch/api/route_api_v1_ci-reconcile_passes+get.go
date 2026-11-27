package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/ci-reconcile/passes",
	Auth:        authAny,
	Description: "The CI reconcile passes of the last 24 hours, newest first: each one's id, the server process that ran it, when it started and ended, what it did (graphql_queries, graphql_points, rest_requests, rest_not_modified, heads_over_rest, records, heads, heads_known) and where it left the cursor, which is where the pass after it takes the order up: the repository, from (the number naming the page of its open pull requests, one above the last head of the page before it), number (the first pull request of that page the pass did not finish, 0 for the whole page), rest (the page is read from the repository's REST listing rather than from a query) and counted (its heads are already among the heads the cycle knows the repository has). Both the counts and the cursor are null until the pass ends. state is running, complete, deadline (its own five-minute bound cut it short), rate_limit (a shared GitHub limit ended it), cancelled (its process shut down under it) or ended_without_completing, which is a pass whose process died before it could record its end. Rows are kept for 48 hours and pruned by the pass that ends.",
	Handler:     (*server).listCIReconcilePasses,
	Successor:   successorIs(http.MethodGet, "/ci-reconcile-passes"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []cireconcile.PassRecord{}},
})
