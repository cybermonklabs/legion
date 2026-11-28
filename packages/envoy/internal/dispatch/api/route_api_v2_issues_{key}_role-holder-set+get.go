package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/issues/{key}/role-holder-set", Auth: authAny, Versions: onlyV2,
		Description: "The session that holds each Legion role of this issue, as the Legion daemon last published " +
			"it: " + issueRoleHoldersText + ". A closed issue's read too. 404 ISSUE_NOT_FOUND for an issue that does " +
			"not exist.",
		Handler:  s.getIssueRoleHolders,
		Resource: "role-holder-set", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: issueRoleHolders{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
	}
})
