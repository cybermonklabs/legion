package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/approval-requests", Auth: authAny, Versions: onlyV2,
		Description: "Open an approval ask for a document's latest version {summary?, include_verification?, actor?}, " +
			"any authenticated caller: include_verification puts the issue's Verification document beside the spec, each " +
			"at its own current version (409 NO_VERIFICATION_DOCUMENT when it has none, 409 NOT_SPEC_DOCUMENT on any document " +
			"but the issue's spec). A document a person approved before carries that approval on the ask. An open request " +
			"over a document it names is renewed or handed back to its person rather than a second one opened, and a " +
			"document approved throughout at its current version needs none. Answers {ask, artifact_id, version, approval, " +
			"coverage, coverage_unread?}: 201 when the request wrote anything, 200 when it stood as asked (ask null when " +
			"nothing is asked); coverage is one line per requirement of the spec with no check, or `the spec marks no " +
			"requirements`, and refuses nothing. 409 PLAN_APPROVED_BY_REVIEW for a plan, 409 NO_VERSION, 409 ISSUE_CLOSED, " +
			"400 SUMMARY_INPUT for a blank summary, 400 NOT_DOCUMENT, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.requestArtifactApprovalV2,
		Resource: "approval-requests", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyJSON, Type: approvalRequestInput{}, Optional: true},
		Shape: routeShape{
			Status: http.StatusCreated, Response: approvalRequestV2{},
			Also: []routeAnswer{{Status: http.StatusOK, Body: approvalRequestV2{}}},
		},
		Scope: scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, artifactNotDocumentRefusal,
			valuePastBoundRefusal, noSettledVersionRefusal,
			{Condition: apicode.ApprovalWaitsOnPerson, Code: apicode.ApprovalWaitsOnHuman},
			{Condition: apicode.NotTheSpec, Code: apicode.NotSpecDocument},
			{Condition: apicode.VerificationDocumentMissing, Code: apicode.NoVerificationDocument},
			{Condition: apicode.ApprovalOfPlan, Code: apicode.PlanApprovedByReview},
			{Condition: apicode.SummaryBlank, Code: apicode.SummaryInput},
		},
	}
})
