package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/spec-form",
	Auth:        authAny,
	Description: "Read the form of The Spec in one version of a document, with no side effect: ?version=N, or the latest settled version. The answer names the scope, the root, the part the document plays, the count of each kind of text and each finding. The scope is covered, list, no_root, root, outside or not_spec. It lists the findings of a document out of scope too. refuses_approval says if Dispatch refuses an approval request or an approval of that version. unread names each approved document under the root whose approved version Dispatch cannot read, so that no rule compared its texts. A version that the document does not have is 404 VERSION_NOT_FOUND.",
	Handler:     (*server).getArtifactSpecForm,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusOK, Response: specFormRead{}, Params: specFormQuery{},
	},
})
