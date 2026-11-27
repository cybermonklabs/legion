package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/edits", Auth: authAny, Versions: onlyV2,
		Description: "Apply a batch of quote- and block-anchored edit ops to a document {ops, summary?, precondition?, " +
			"actor?}, a bearer naming its session in actor: each op {op, find?, with?, occurrence?, markdown?, after?, " +
			"before?, block?, index?, type?, attributes?}; precondition {document?, blocks?} holds the batch to the tokens " +
			"the caller read. A batch that changed the document writes a version, named with the summary, else a snapshot, " +
			"and appends artifact.version. Answers 200 {applied, changed, lost_ops, token, unchanged_ops, version, written} " +
			"with advice. 409 PRECONDITION_FAILED {code, error, current, mismatches} for a precondition the document no " +
			"longer meets, 409 EDIT_LOST_TO_CONCURRENT_CHANGE {code, error, lost_ops or suggestion, participants} for text " +
			"a concurrent change removed first, 409 TARGET_AMBIGUOUS {code, error, candidates} for a quote found in several " +
			"places, 409 ISSUE_CLOSED, 400 INVALID_OP, 400 REPLACE_FIELDS for a replace that names no with or carries a " +
			"member a replace does not take (it takes find, with and occurrence), 400 NOT_DOCUMENT, 404 ARTIFACT_NOT_FOUND. " +
			specWriterDocumentText("an edit of its spec or Verification document"),
		Handler:  s.editArtifactV2,
		Resource: "edits", Kind: kindCollection,
		Body: requestBody{Kind: bodyJSON, Type: editArtifactInput{}},
		Shape: routeShape{
			Status: http.StatusOK, Response: editArtifactResponse{}, Advice: writeAdvice{},
			// A document of no issue, or one whose issue advice could not be read, carries the
			// document half alone (applyEdits, the withRawAdvice arm).
			OtherAdvice: []any{documentWriteAdvice{}},
			Refusals: []routeAnswer{
				{Status: http.StatusConflict, Body: editPreconditionError{}},
				{Status: http.StatusConflict, Body: editLostError{}},
				{Status: http.StatusConflict, Body: ambiguousTargetError{}},
			},
		},
		Scope: scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: documentRefusals(
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, artifactNotDocumentRefusal,
			documentPreconditionRefusal, quoteAmbiguousRefusal, editLostRefusal, targetMissingRefusal,
			askBlockRefusedRefusal, ciCheckRuleBrokenRefusal, documentPastSizeBoundRefusal,
			apicode.Refusal{Condition: apicode.EditOpInvalid, Code: apicode.InvalidOp},
			apicode.Refusal{Condition: apicode.ReplaceFieldsInvalid, Code: apicode.ReplaceFields},
			apicode.Refusal{Condition: apicode.QuoteSpansBlocks, Code: apicode.TargetSpansBlocks},
			apicode.Refusal{Condition: apicode.TableRowsTooWide, Code: apicode.TableWidth},
			apicode.Refusal{Condition: apicode.EditPreconditionMalformed, Code: apicode.InvalidPrecondition},
			apicode.Refusal{Condition: apicode.ConditionalEditsQueued, Code: apicode.EditQueueFull},
			specWriterNotArchitectRefusal,
		),
	}
})
