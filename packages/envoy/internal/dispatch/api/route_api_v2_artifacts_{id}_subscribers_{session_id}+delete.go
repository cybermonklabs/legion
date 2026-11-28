package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/artifacts/{id}/subscribers/{session_id}", Auth: authAny, Versions: onlyV2,
		Description: "Unsubscribe a session from a project document's topics (the person scope): the topics scoped to this " +
			"document alone are removed from the session's Envoy interests, and subscription.remove_requested and then " +
			"subscription.removed are appended on the document. 204. 409 NOT_REMOVABLE where only a broader topic matches, " +
			"404 SUBSCRIBER_NOT_FOUND for a session that is not subscribed, 502 ENVOY_UNSUBSCRIBE_FAILED, 503 " +
			"ENVOY_UNAVAILABLE, 400 ARTIFACT_LINKED or ARTIFACT_AGENT_OWNED, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.unsubscribeArtifactSession,
		Resource: "subscribers", Kind: kindItem, Key: "{session_id}",
		Shape:       routeShape{Status: http.StatusNoContent},
		OAuthScopes: v2PersonScope,
		Scope:       scopeProject, ProjectOf: projectPathArtifact, SessionPath: "session_id",
		IDs:      v2DocumentIDs(),
		Refusals: unsubscribeRefusals(projectDocumentRefusals(undeclaredKeyRefusal)...),
	}
})
