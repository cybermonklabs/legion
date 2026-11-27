package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/subscribers", Auth: authAny, Versions: onlyV2,
		Description: "The sessions subscribed to a project document's topics (the person scope), newest heartbeat first: " +
			"each {session_id, title, live, state?, last_seen, topics, removable, via?}, removable false where only a " +
			"broader topic matches, which via names. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. " +
			"An issue's document is 400 ARTIFACT_LINKED, a conversation's file 400 ARTIFACT_AGENT_OWNED; 503 " +
			"ENVOY_UNAVAILABLE or ENVOY_REFILLING when the listener cannot answer; 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.listArtifactSubscribersV2,
		Resource: "subscribers", Kind: kindCollection, Key: "{session_id}",
		Query:       artifactSubscribersV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[subscriber]{}},
		OAuthScopes: v2PersonScope,
		IDs:         v2DocumentIDs(),
		Refusals:    listRefusals(projectDocumentRefusals(envoyListenerRefusals()...)...),
	}
})
