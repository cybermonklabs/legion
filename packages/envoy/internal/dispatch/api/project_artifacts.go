package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
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
	query := `
		select ` + docs.ArtifactColumns + `
		from artifacts
		where project_key = $1 and not is_primary`
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	s.writeArtifactList(w, r, query, project)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
