package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/results/dry-run", Auth: authAny, Versions: onlyV2,
		Description: "The dry run of a result's record: the body POST /api/v2/issues/{key}/results takes, refused as " +
			"the record refuses it before it writes (400 RESULT_INPUT, 400 RESULT_CONTRADICTS, 400 UNKNOWN_CHECK, 409 " +
			"CHECK_AMBIGUOUS, 409 CHECK_PROVED_BY_CI, 400 RESULT_COMMIT_UNKNOWN, 502 GITHUB_UNREADABLE), and otherwise " +
			"answered 200 {line}, " +
			"the pull-request body line the record would answer, with nothing recorded and no event appended. A closed " +
			"issue, a screenshot and a check reworded while the record writes are refused by the record alone. 404 " +
			"ISSUE_NOT_FOUND.",
		Handler:  s.dryRunIssueResultV2,
		Resource: "dry-run", Kind: kindComputation,
		Body:  requestBody{Kind: bodyJSON, Type: checkResultInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: checkResultDryRun{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: checkResultRefusals(),
	}
})
