package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/settings/new-issue", Auth: authAny, Versions: onlyV2,
		Description: "What the dashboard's search bar files a person's unmatched typed words as: {label, session}, the " +
			"label that new issue carries and the session it is routed to and its first message goes to, as this " +
			"deployment configured them (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION). Each is \"\" where it " +
			"configured none: no label on the issue, and no conversation started on it. The bar writes the issue itself, " +
			"through POST /api/v2/issues and the issue's own artifact, patch and message routes.",
		Handler:  s.getNewIssueSettings,
		Resource: "new-issue", Kind: kindSingleton,
		Shape:    routeShape{Status: http.StatusOK, Response: newIssueSettings{}},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal},
	}
})
