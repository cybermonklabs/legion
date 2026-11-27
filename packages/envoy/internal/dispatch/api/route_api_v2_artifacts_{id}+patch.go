package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Correct a document's title {name} (the person scope). It writes the name alone - the slug, the " +
			"reference key it gives and the role the document plays do not move - appends artifact.renamed, and answers " +
			"the artifact as GET /api/v2/artifacts/{id} reads it. A name another of the owner's artifacts holds by name or " +
			"slug is 409 ARTIFACT_NAME_TAKEN, a name whose slug is one of the documents Dispatch makes itself, on a document " +
			"that is not that one, 400 ARTIFACT_ROLE_NAME, the issue's spec 400 ARTIFACT_PRIMARY, an artifact that is no " +
			"document 400 ARTIFACT_KIND_MISMATCH, a name that is empty or too long 400 INVALID_ARTIFACT, a closed issue's " +
			"document 409 ISSUE_CLOSED; 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.renameArtifactV2,
		Resource: "artifacts", Kind: kindItem, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: renameArtifactInput{}},
		Shape:       routeShape{Status: http.StatusOK, Response: artifactV2{}},
		OAuthScopes: v2PersonScope,
		Scope:       scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, roleDocumentNameRefusal,
			{Condition: apicode.ArtifactNameHeld, Code: apicode.ArtifactNameTaken},
			{Condition: apicode.SpecRenamed, Code: apicode.ArtifactPrimary},
			{Condition: apicode.RenamedNotDocument, Code: apicode.ArtifactKindMismatch},
			{Condition: apicode.ArtifactNameInvalid, Code: apicode.InvalidArtifact},
		},
	}
})
