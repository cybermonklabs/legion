package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/me/marks/{kind}/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Write the caller's mark of one kind on one object, the fields the body names under that kind's " +
			"rules, and answer the mark. The body's fields are the path's kind's: " + markWriteText + " A kind that is " +
			"none of the six is 400 INVALID_KIND. Needs the person scope, which a project-scoped session's token does " +
			"not hold. A credential that proved a session, a token the secrets broker issued, writes the agent mark of " +
			"that session alone: an agent mark of another session is 403 SESSION_SCOPE. The target must exist: 404 " +
			"ISSUE_NOT_FOUND, ARTIFACT_NOT_FOUND, THREAD_NOT_FOUND, MESSAGE_NOT_FOUND or ASK_NOT_FOUND by kind; an " +
			"agent mark's session is not held to exist.",
		Handler:  s.patchMarkV2,
		Resource: "marks", Kind: kindItem, Key: "{kind}/{id}",
		Body: requestBody{Kind: bodyKindUnion, Arms: markArms},
		Shape: routeShape{
			Status: http.StatusOK, Response: markV2{},
			Refusals: []routeAnswer{{Status: http.StatusConflict, Body: markStaleV2{}}},
		},
		OAuthScopes: v2PersonScope,
		Scope:       scopeSession,
		IDs:         v2MarkIDs(),
		Refusals:    markWriteRefusals(),
	}
})
