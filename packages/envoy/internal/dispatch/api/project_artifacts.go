package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// listProjectArtifacts answers a project's documents. A project its reader does not see is a
// project the read does not find (loadProject), and each document is read through
// access.DocumentClause for that same reader, as every read of an artifact is.
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
		where project_key = $1 and not is_primary and ` + access.DocumentClause("artifacts", "$2")
	if unlinked {
		query += ` and issue_key is null`
	}
	query += ` order by created_at, id`
	rows, err := s.deps.Store.Pool.Query(r.Context(), query, project, refs.VisibilityArg(who))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := loadArtifactDetails(r.Context(), s.deps.Store.Pool, who, artifactPointers(artifacts)); err != nil {
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
