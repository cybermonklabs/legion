package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/spec-form",
	Auth:        authAny,
	Description: "Read the form of The Spec in one version of a document, with no side effect: ?version=N, or the latest settled version. The answer names the scope, the root, the part the document plays, the count of each kind of text and the findings: findings holds at most 500 of them, and findings_total counts them all. finding_lines are the lines that list those findings, as dispatch spec-form prints them: a head that counts the texts that break the form when Dispatch refuses the approval, then one line for each finding, as the refusal of an approval request writes it, then a line that says how many it lists when it lists fewer than findings_total. The scope is covered, list, no_root, root, outside or not_spec. It lists the findings of a document out of scope too. refuses_approval says if Dispatch refuses an approval request or an approval of that version. unread names each approved document under the root whose approved version Dispatch cannot read, so that no rule compared its texts. A version that the document does not have is 404 VERSION_NOT_FOUND.",
	Handler:     (*server).getArtifactSpecForm,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: specFormRead{}, Params: specFormQuery{},
	},
})
