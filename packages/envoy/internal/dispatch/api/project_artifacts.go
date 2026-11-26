package api

import (
	"net/http"
)

func (s *server) listProjectArtifacts(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	project := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, who, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	unlinked := r.URL.Query().Get("unlinked") == "true"
	query := `
		select ` + artifactColumns + `
		from artifacts
		where project_key = $1 and not is_primary`
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	s.writeArtifactList(w, r, who, query, project)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
