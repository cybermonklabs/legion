package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specform"
)

// specFormQuery is GET /api/v1/artifacts/{id}/spec-form's query.
type specFormQuery struct {
	// Version is the version to read, a whole number of 1 or more. Absent, it is the latest
	// settled version, which an approval request would name.
	Version int `query:"version"`
}

// GET /api/v1/artifacts/{id}/spec-form[?version=N]  (any authenticated caller)
//
// The form of The Spec of one version of a document, with no side effect: where the document
// stands (its scope, its root and the part it plays), the count of each kind of text it holds,
// and its findings, which it lists for a document out of scope too: at most specFormFindingsHeld
// of them, as a refusal's body holds, and the count of all of them. finding_lines are the lines
// that list them (specform.Listing), which `dispatch spec-form` prints as they are, so the words
// of each finding's line are the refusal's own. refuses_approval says whether the form gate
// refuses an approval request or an approval of that version. The judgment runs on the request's
// context, so a caller that hangs up stops it.
func (s *server) getArtifactSpecForm(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	pool := s.deps.Store.Pool
	artifact, err := s.loadArtifact(r.Context(), pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "only a document holds texts of The Spec")
		return
	}
	version, present, valid := parseQueryInt(r.URL.Query(), "version", 1, maxApprovalVersion)
	if present && !valid {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "version must be a whole number of 1 or more")
		return
	}
	if !present {
		if version, err = settledVersionNumber(r.Context(), pool, artifact.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if version == 0 {
			writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to read yet")
			return
		}
	}
	result, err := s.deps.SpecForm.specFormOf(r.Context(), pool, []model.AskApprovalDocument{{ArtifactID: artifact.ID, Name: artifact.Name, Version: version}}, actRead)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	document := result.documents[0]
	plays := ""
	switch {
	case isSpecDocument(artifact):
		plays = "spec"
	case documentPlays(artifact, model.ArtifactRoleVerification):
		plays = model.ArtifactRoleVerification
	}
	findings := document.findings[:min(len(document.findings), specFormFindingsHeld)]
	refuses := document.inScope() && len(document.findings) > 0
	WriteJSON(w, http.StatusOK, specFormRead{
		ArtifactID:      artifact.ID,
		Name:            artifact.Name,
		Version:         version,
		InScope:         document.inScope(),
		Scope:           document.scope,
		Plays:           plays,
		Root:            document.root,
		Texts:           document.texts,
		Findings:        findings,
		FindingsTotal:   len(document.findings),
		FindingLines:    specform.Listing(artifact.Name, version, refuses, plainFindings(findings), len(document.findings)),
		Terms:           []specFormTerm{},
		Unread:          document.unread,
		RefusesApproval: refuses,
	})
}
