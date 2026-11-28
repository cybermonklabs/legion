package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Remove a document of an issue that is still untouched: made by typing a title and never written " +
			"in (the person scope). It answers 204 and appends artifact.removed on the issue, and it leaves no row behind. " +
			"A document that holds writing or a later version, an edit that has not settled, or a comment, ask, review, " +
			"approval request, check result, read mark or link from anything in Dispatch is 409 ARTIFACT_NOT_UNTOUCHED, " +
			"saying which of those it is; the issue's spec, a document that plays a role and an artifact that is no " +
			"document read the same refusal; a project's document is 400 ARTIFACT_PROJECT_DOCUMENT; a closed issue's 409 " +
			"ISSUE_CLOSED; 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.removeArtifact,
		Resource: "artifacts", Kind: kindItem, Key: "{id}",
		Shape:       routeShape{Status: http.StatusNoContent},
		OAuthScopes: v2PersonScope,
		Scope:       scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal,
			{Condition: apicode.ArtifactTouched, Code: apicode.ArtifactNotUntouched},
			{Condition: apicode.ProjectDocumentRemoved, Code: apicode.ArtifactProjectDocument},
		},
	}
})
