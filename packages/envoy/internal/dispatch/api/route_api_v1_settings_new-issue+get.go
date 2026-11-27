package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/settings/new-issue",
	Auth:        authAny,
	Description: "What the command bar files a person's words as a new issue with: {label, session}, the label that new issue carries and the session it is routed to, as this deployment configured them (DISPATCH_NEW_ISSUE_LABEL, DISPATCH_NEW_ISSUE_SESSION). Each is \"\" where it configured none: no label on the issue, and no route. Two readers file with it. The bar's File as a new issue starts a new_issue run (POST /api/v1/going/runs with kind new_issue), whose create carries both. Where no deciding model is configured, the bar's New issue group writes the issue itself: POST /api/v1/issues with the label, POST /api/v1/issues/{key}/artifacts with its Verification document, and PATCH /api/v1/issues/{key} with the route session:<id>. Neither posts a message on the issue, and neither sends the session a first message: a message to an agent is a change of a plan the person approves.",
	Handler:     (*server).getNewIssueSettings,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: newIssueSettings{}},
})
