package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// listRepoProjects answers the rows of repo_projects as the last release left them, of the
// projects the caller sees: a mapping to a project it does not see is one the list does not hold
// (access.ProjectClause). Nothing writes them now: a project's default repository is a column of
// the project, and the rename pairs are read once at boot (store.ReadRepoRenames). The route stays
// for one release because the Legion daemons of the last release read it at boot.
func (s *server) listRepoProjects(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+repoProjectColumns+`
		from repo_projects
		where `+access.ProjectClause("project", "$1")+`
		order by repo
	`, refs.VisibilityArg(who))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()

	mappings := []model.RepoProject{}
	for rows.Next() {
		mapping, err := scanRepoProject(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		mappings = append(mappings, mapping)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, mappings)
}

// repoProjectColumns are the columns scanRepoProject reads, in its order.
const repoProjectColumns = `repo, project, is_default, created_by, created_at, renamed_to`

func scanRepoProject(row rowScanner) (model.RepoProject, error) {
	var mapping model.RepoProject
	var createdBy []byte
	if err := row.Scan(&mapping.Repo, &mapping.Project, &mapping.Default, &createdBy, &mapping.CreatedAt, &mapping.RenamedTo); err != nil {
		return model.RepoProject{}, err
	}
	if err := json.Unmarshal(createdBy, &mapping.CreatedBy); err != nil {
		return model.RepoProject{}, fmt.Errorf("decode repository project author: %w", err)
	}
	return mapping, nil
}
