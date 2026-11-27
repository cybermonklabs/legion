package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/marks/{kind}/{id}", Auth: authAny, Versions: onlyV2,
		Description: "The caller's mark of one kind on one object, its target's default state where the caller has " +
			"written none. " + markText + ". A kind that is none of the six is 400 INVALID_KIND. Needs the person " +
			"scope. The target must exist: 404 ISSUE_NOT_FOUND, ARTIFACT_NOT_FOUND, THREAD_NOT_FOUND (an id that is no " +
			"ask, root comment or root message), MESSAGE_NOT_FOUND or ASK_NOT_FOUND by kind; an agent mark's session " +
			"is not held to exist.",
		Handler:  s.getMarkV2,
		Resource: "marks", Kind: kindItem, Key: "{kind}/{id}",
		Shape:       routeShape{Status: http.StatusOK, Response: markV2{}},
		OAuthScopes: v2PersonScope,
		IDs:         v2MarkIDs(),
		Refusals:    markTargetRefusals(),
	}
})
