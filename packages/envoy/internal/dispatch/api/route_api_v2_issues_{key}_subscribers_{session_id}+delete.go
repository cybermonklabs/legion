package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/issues/{key}/subscribers/{session_id}", Auth: authAny, Versions: onlyV2,
		Description: "Unsubscribe a session from an issue's topics (the person scope): the topics scoped to this issue " +
			"alone are removed from the session's Envoy interests, and subscription.remove_requested and then " +
			"subscription.removed are appended on the issue. 204. 409 NOT_REMOVABLE where only a broader topic matches, " +
			"404 SUBSCRIBER_NOT_FOUND for a session that is not subscribed, 502 ENVOY_UNSUBSCRIBE_FAILED, 503 " +
			"ENVOY_UNAVAILABLE, 404 ISSUE_NOT_FOUND.",
		Handler:  s.unsubscribeIssueSessionV2,
		Resource: "subscribers", Kind: kindItem, Key: "{session_id}",
		Shape:       routeShape{Status: http.StatusNoContent},
		OAuthScopes: v2PersonScope,
		Scope:       scopeProject, ProjectOf: projectPathIssue, SessionPath: "session_id",
		Refusals: unsubscribeRefusals(undeclaredKeyRefusal, issueMissingRefusal),
	}
})
