package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues", Auth: authAny, Versions: onlyV2,
		Description: "List the issues of every project, filtered. project=<key>, repeatable, keeps those projects' " +
			"issues, with no default project; GET /api/v2/projects/{key}/issues lists one project's. ref=<KEY or " +
			"owner/repo#n> keeps the issue a key names, or every issue linked to that GitHub issue or pull request (an " +
			"issue imported from it, or one a link names), none where nothing is; any other ref is 400 " +
			"INVALID_ISSUE_REF. " + issueListText,
		Handler:  s.listIssuesV2,
		Resource: "issues", Kind: kindCollection, Key: "{key}",
		TimeFields: v2IssueTimes, CrossProject: true,
		Query:       issueListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[model.IssueSummary]{}},
		List:        v2IssueList,
		Authorities: []authorityRule{issuePinnedAuthority},
		Refusals:    issueListRefusals(issueRefInvalidRefusal),
	}
})
