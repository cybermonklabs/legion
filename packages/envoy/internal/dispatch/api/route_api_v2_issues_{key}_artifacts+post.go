package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "Upload an issue document or file. " + artifactUploadText + " " + artifactUploadPreconditionText +
			" " + artifactUploadRoleText + " " + artifactUploadAnswerText + " " + artifactUploadRefusals("") +
			" The write's advice rides the answer. 409 ISSUE_CLOSED for a closed issue and 404 ISSUE_NOT_FOUND for one " +
			"that does not exist, both decided before the body is stored to disk or parsed; the body is still read and " +
			"discarded before the answer, and a body past the upload's bound is answered 413 REQUEST_TOO_LARGE instead. " +
			specWriterDocumentText("an upload of its spec or Verification document, a file under the name verification.md among them,"),
		Handler:  s.uploadIssueArtifactV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
		// An issue document whose issue advice could not be read carries the document half
		// alone (storeArtifact, the withRawAdvice arm).
		Shape: routeShape{
			Status: http.StatusCreated, Response: artifactUploadV2{}, Advice: writeAdvice{},
			OtherAdvice: []any{documentWriteAdvice{}}, Refusals: uploadPreconditionRefusalAnswers(),
		},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		RefusesClosedOwner: true,
		Refusals: documentContentRefusals(
			uploadBodyRefusals(issueMissingRefusal, documentPreconditionRefusal, roleDocumentNameRefusal, specWriterNotArchitectRefusal)...,
		),
	}
})
