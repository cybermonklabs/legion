package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The repository answer is the list of the repositories a project's issues resolve to, which the
// secrets broker relays to the edge listener, so that the edge relays to an outside person's agents
// the GitHub events of exactly those repositories. It is computed from the project's issues by the
// server's one repository-resolution rule (refs.ProjectRepositories), never from a list of a
// project's repositories. It is plain data read outside any transaction, and its read class is the
// membership answer's, accessAccount: only a service account the deployment names in
// DISPATCH_ACCESS_ACCOUNTS reads it, with its own projected token, and every other caller is
// refused before anything is read (requireAccessAccount).

// projectRepositoriesAnswer is GET /api/v1/access/projects/{key}/repositories' body: the project's
// key and its repositories, each `owner/name` once, sorted byte-wise, and [] when no issue of the
// project resolves to one, never null or left out.
type projectRepositoriesAnswer struct {
	Project      string   `json:"project"`
	Repositories []string `json:"repositories" nullable:"false"`
}

// codeRepositoriesUnresolved refuses the repository answer of a project one of whose issues has a
// parent chain the walk could not finish (refs.UnresolvedChainError).
const codeRepositoriesUnresolved = "REPOSITORIES_UNRESOLVED"

// getAccessProjectRepositories answers project {key}'s repositories, read live on each request:
// 404 PROJECT_NOT_FOUND for an unknown key, and 503 REPOSITORIES_UNRESOLVED naming the issue whose
// chain the walk could not finish, rather than a shorter list. That refusal reaches only the secrets
// broker, which relays no repository of the project while it stands, so it is also logged at WARN
// with the project and the issue for the operator who must repair the chain.
func (s *server) getAccessProjectRepositories(w http.ResponseWriter, r *http.Request) {
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	project := r.PathValue("key")
	repositories, err := refs.ProjectRepositories(r.Context(), pool, project)
	if unresolved := (*refs.UnresolvedChainError)(nil); errors.As(err, &unresolved) {
		slog.Warn("dispatch: the repository answer is unresolved", "project", unresolved.Project, "issue", unresolved.Issue)
		writeError(w, codeRepositoriesUnresolved, http.StatusServiceUnavailable, unresolved.Error()+
			", so no list is answered rather than one that leaves out a repository its ancestors may name")
		return
	}
	if err != nil {
		if errors.Is(err, access.ErrProjectNotFound) {
			writeError(w, "PROJECT_NOT_FOUND", http.StatusNotFound, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, projectRepositoriesAnswer{Project: project, Repositories: repositories})
}
