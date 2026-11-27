package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/settings/new-issue",
	Auth:        authAny,
	Description: "What the dashboard's search bar files a person's unmatched typed words as: {label, session}, the label that new issue carries and the session it is routed to and its first message goes to, as this deployment configured them (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION). Each is \"\" where it configured none: no label on the issue, and no conversation started on it. The bar writes the issue itself, through POST /api/v1/issues and the issue's own artifact, patch and message routes.",
	Handler:     (*server).getNewIssueSettings,
	Successor:   successorIs(http.MethodGet, "/settings/new-issue"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: newIssueSettings{}},
})
