package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/results/dry-run",
	Auth:        authAny,
	Description: "The dry run of a result's record: the record's body, refused as the record refuses it before it writes (400 RESULT_INPUT, 400 RESULT_CONTRADICTS, 400 UNKNOWN_CHECK, 409 CHECK_AMBIGUOUS, 409 CHECK_PROVED_BY_CI, 400 RESULT_COMMIT_UNKNOWN), and otherwise answered {line}, the pull-request body line the record would answer, with nothing recorded and no event appended. A closed issue, a screenshot and a check reworded while the record writes are refused by the record alone.",
	Handler:     (*server).dryRunCheckResult,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: checkResultDryRun{}, Request: checkResultInput{},
	},
})
