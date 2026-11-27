package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/evals/{slug}",
	Auth:        authAny,
	Description: "One eval's versions, newest first, one page {items, total, limit, offset} of the list's item shape, each with charts, the version's declared charts ([] for none), which no list read answers: the newest states the window's values as the list read does (the same window, compare range, repo and freshness, repo held to the list read's bounds), every older version its definition alone; and doc_text, the method the eval's section of the project's evals-md document holds (null where the document holds none). 400 INVALID_QUERY names a malformed key, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND an unknown project and EVAL_NOT_FOUND a slug no eval of it holds.",
	Handler:     (*server).getProjectEval,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: "The eval's method is read from the project's evals-md document through the document service (evals.Store.Method). " + docsCompactionNote},
	Shape: routeShape{
		Status: http.StatusOK, Response: evalDetailResponse{}, Params: evalDetailQuery{},
	},
})
