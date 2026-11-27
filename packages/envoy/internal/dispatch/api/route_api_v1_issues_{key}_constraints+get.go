package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/constraints",
	Auth:        authAny,
	Description: "The constraints under the issue the caller may see, in the order they were written there: {issue, constraints}, each with its address dispatch://constraint/<uuid>, id, standard, reference, measurement, value and taken_where (null where it holds none), author (the person or session its authored_by tuple names: the session the body named for one written on the shared bearer, null where the write named only a workload), created_at, state and visibility. A constraint reserved for its author is in its author's list alone, which reads its visibility author, and a constraint that is gone is in no list. It takes no query. An issue Dispatch does not hold is 404 NOT_FOUND.",
	Handler:     (*server).listIssueConstraints,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueConstraints{}},
})
