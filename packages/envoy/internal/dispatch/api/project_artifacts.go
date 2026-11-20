package api

import (
	"net/http"
)

func (s *server) listProjectArtifacts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	unlinked := r.URL.Query().Get("unlinked") == "true"
	// A retired document is in no list of its project's documents (docs.RetireDocument).
	query := `
		select ` + artifactColumns + `
		from artifacts
		where project_key = $1 and not is_primary and retired_at is null`
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	s.writeArtifactList(w, r, query, project)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
