package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// every finding, which it lists for a document out of scope too, and the list of terms of its
// root with each approved version that Dispatch could not read as it made the list
// (specFormList), this document's own among them. refuses_approval says whether the form gate
// refuses an approval request or an approval of that version.
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
	result, err := s.specFormOf(r.Context(), pool, []model.AskApprovalDocument{{ArtifactID: artifact.ID, Name: artifact.Name, Version: version}}, actRead)
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
	WriteJSON(w, http.StatusOK, specFormRead{
		ArtifactID:      artifact.ID,
		Name:            artifact.Name,
		Version:         version,
		InScope:         document.inScope(),
		Scope:           document.scope,
		Plays:           plays,
		Root:            document.root,
		Texts:           document.texts,
		Findings:        document.findings,
		FindingsTotal:   len(document.findings),
		specFormList:    document.list,
		RefusesApproval: document.inScope() && len(document.findings) > 0,
	})
}
