package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/rebuilds", Auth: authAny, Versions: onlyV2,
		Description: "Rebuild a document history ygo cannot load (the person scope) {markdown?}: the history is replaced " +
			"by one seed update from the latest saved version, or from the markdown given, which writes a version and " +
			"appends artifact.version where it differs from that version. Answers 200 the report {head, " +
			"removed_checkpoints, removed_snapshots, removed_updates, source_version, validation_error}. A document a room " +
			"holds open is 409 DOCUMENT_LIVE, one whose history loads 409 DOCUMENT_LOADS, markdown given to a closed issue's " +
			"document 409 ISSUE_CLOSED, a file or an image 400 NOT_DOCUMENT; 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.rebuildArtifactV2,
		Resource: "rebuilds", Kind: kindCollection,
		Body:        requestBody{Kind: bodyJSON, Type: rebuildArtifactInput{}},
		Shape:       routeShape{Status: http.StatusOK, Response: docs.RebuildReport{}},
		OAuthScopes: v2PersonScope,
		Scope:       scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		// A rebuild replaces a history that does not load, so it answers no DOCUMENT_UNLOADABLE,
		// and it parses its source afresh, so no stored tree outside the schema is its refusal.
		Refusals: markdownWriteRefusals(
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, artifactNotDocumentRefusal,
			documentServiceDownRefusal, documentNewerContentRefusal,
			apicode.Refusal{Condition: apicode.DocumentOpenInRoom, Code: apicode.DocumentLive},
			apicode.Refusal{Condition: apicode.DocumentHistoryLoads, Code: apicode.DocumentLoads},
		),
	}
})
