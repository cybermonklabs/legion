package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/versions", Auth: authAny, Versions: onlyV2,
		Description: "Name a document's current version {summary, actor?}, a bearer naming its session in actor: a new " +
			"named version of the live document, and artifact.version appended. Answers 201 the version. 400 " +
			"INVALID_VERSION for a blank summary, 400 NOT_DOCUMENT, 409 ISSUE_CLOSED, 404 ARTIFACT_NOT_FOUND. " +
			specWriterDocumentText("a named version of its spec or Verification document"),
		Handler:  s.createNamedVersionV2,
		Resource: "versions", Kind: kindCollection, Key: "{number}",
		Body:  requestBody{Kind: bodyJSON, Type: createVersionInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: model.Version{}},
		Scope: scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: documentRefusals(
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, artifactNotDocumentRefusal,
			apicode.Refusal{Condition: apicode.VersionSummaryBlank, Code: apicode.InvalidVersion},
			specWriterNotArchitectRefusal,
		),
	}
})
