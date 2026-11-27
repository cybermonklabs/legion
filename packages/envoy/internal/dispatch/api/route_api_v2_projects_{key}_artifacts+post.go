package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/projects/{key}/artifacts", Auth: authAny, Versions: onlyV2,
		Description: "Upload a project document or file, which no issue holds; its events are its own. " +
			artifactUploadText + " " + artifactUploadPreconditionText + " " + artifactUploadAnswerText + " " +
			artifactUploadRefusals("") + " 404 PROJECT_NOT_FOUND for a project that does " +
			"not exist.",
		Handler:  s.uploadProjectArtifactV2,
		Resource: "artifacts", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
		// A project document has no issue state to report, so its write advice is the document
		// half alone (storeArtifact, the withRawAdvice arm).
		Shape: routeShape{
			Status: http.StatusCreated, Response: artifactUploadV2{}, Advice: documentWriteAdvice{},
			Refusals: uploadPreconditionRefusalAnswers(),
		},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: documentContentRefusals(uploadBodyRefusals(projectMissingRefusal, documentPreconditionRefusal)...),
	}
})
