package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
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
	// A retired document is in no list of its project's documents (docs.RetireDocument), and an
	// issue's tree artifact and its decisions query are in none either, as they are in no list of
	// the issue's own (issueArtifactsStatement): the tree is the issue's room and Decisions a view
	// over decision nodes, neither of them a document.
	query := `
		select ` + artifactColumns + `
		from artifacts
		where project_key = $1 and not is_primary and retired_at is null
		  and kind <> 'tree' and not ` + refs.DecisionsQuerySQL("artifacts")
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	rows, err := s.deps.Store.Pool.Query(r.Context(), query, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := loadArtifactDetails(r.Context(), s.deps.Store.Pool, artifactPointers(artifacts), artifactVersionsStatement); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	reads := artifactReads(artifacts, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, reads)
}

func (s *server) uploadProjectArtifact(w http.ResponseWriter, r *http.Request) {
	s.uploadArtifactFor(w, r, artifactTarget{Project: r.PathValue("key")})
}
