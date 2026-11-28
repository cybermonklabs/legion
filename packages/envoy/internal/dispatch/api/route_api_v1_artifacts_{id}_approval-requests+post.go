package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/approval-requests",
	Auth:        authAny,
	Description: "Open an approval ask for a document's latest version, with an optional summary, and with include_verification for the issue's Verification document beside it (409 NO_VERIFICATION_DOCUMENT when it has none), each at its own current version; the answer reviews every document the ask names. include_verification is for the issue's spec alone: on any other document, the Verification document itself included, it is 409 NOT_SPEC_DOCUMENT. A document a human approved before carries that approval on the ask (approval.approved: version, at, by). An open ask follows later versions of any document it names and waits on its agent until this route hands the same row back to a human, save that an ask waiting on the human keeps its turn, quietly, over a version a settlement writes that holds the text the ask names, byte for byte or, for a version another renderer version stored or none stamped, as Dispatch renders that text now; a version a route writes (a named version, an upload, an edit) moves it to its agent whatever text it holds. Every answer carries coverage, one line per requirement of the issue's spec with no check, or the single line `the spec marks no requirements` for a spec that marks none (GET /api/v1/issues/{key}/coverage), and none at all for a spec every requirement of which has a check; it is empty for a document that is not a spec, never names a missing plan task, and a gap refuses nothing. A coverage read that fails answers coverage_unread true and records the request all the same. A request is 409 SPEC_FORM when a text of a covered document breaks the form of The Spec (DISPATCH_SPEC_ROOTS). The refusal names each text and its rule. A version that the request names and the parser refuses is a finding of its document (rule unreadable). spec_form says what Dispatch read in a covered document, and its unread names each approved document under the root whose approved version Dispatch cannot read, so that no rule compared its texts.",
	Handler:     (*server).requestArtifactApproval,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: approvalRequestResponse{}, Request: approvalRequestInput{}, OptionalRequest: true,
		// An open request that already stood, or a document already approved throughout,
		// answers the same shape at 200: nothing was appended (reviews.go).
		Also:     []routeAnswer{{Status: http.StatusOK, Body: approvalRequestResponse{}}},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: specFormError{}}},
	},
})
