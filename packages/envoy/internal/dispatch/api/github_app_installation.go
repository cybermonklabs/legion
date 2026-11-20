package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// appInstallationResponse is GET /api/v1/github/app-installation/{owner}/{repo}'s answer.
type appInstallationResponse struct {
	Installed bool `json:"installed"`
}

// getAppInstallation answers whether Dispatch's GitHub App, the one GitHub sends the webhook
// events of its repositories through, is installed on the path's repository: GitHub's
// installation lookup under the App's JWT, its 404 being false. Any authenticated caller may ask,
// whatever its token lists (read class none): whether the App is installed on a repository is no
// project's content, and the Legion daemon asks it for every repository it serves, since on a
// repository the App is not installed on GitHub sends Legion none of its events and nothing
// redelivers them. Dispatch with no App key answers 503 GITHUB_APP_UNCONFIGURED; any other failed
// lookup 502 GITHUB_UNREADABLE, naming the repository and GitHub's error.
func (s *server) getAppInstallation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, name, _ := strings.Cut(repo, "/")
	installed, err := s.deps.GitHub.Installed(r.Context(), owner, name)
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		writeError(w, "GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, "GITHUB_UNREADABLE", http.StatusBadGateway, fmt.Sprintf("read the GitHub App's installation on %s: %v", repo, err))
	default:
		WriteJSON(w, http.StatusOK, appInstallationResponse{Installed: installed})
	}
}

// repositoriesHiddenQuery is refs.RepositoryClause over each repository of an array, each held to
// the GitHub id repository_ids records for it (refs.RepositoryIDSQL), the reader's visibility
// argument bound at $1 and the repositories at $2, each answered with whether it is hidden. Neither
// placeholder differs between reads, so the text is built once.
var repositoriesHiddenQuery = `select asked.repo, not ` + refs.RepositoryClause("asked.repo", refs.RepositoryIDSQL("asked.repo"), "$1") + ` from unnest($2::text[]) as asked(repo)`

// repositoriesHidden answers, for each of repositories (owner/name), whether reader may not see it:
// whether it fails refs.RepositoryClause under the GitHub id repository_ids records for it, read in
// one statement and keyed by the name as given. A repository with no recorded id is hidden from an
// open reader whom no issue it sees gives the repository while a project is private, since the rule
// cannot tell it from a private project's repository under a new name or owner. A reader that sees
// every object (seesEveryObject) sees every repository, and nothing is read for it (R9); nothing is
// read for no repository either. Delivery's pull requests that name no issue and its runs that ship
// none (deliveryPopulation.cutTo) hold a repository to it, as the GitHub proxy does (routes'
// repositoryHidden).
func (s *server) repositoriesHidden(ctx context.Context, reader model.Actor, repositories []string) (map[string]bool, error) {
	hidden := make(map[string]bool, len(repositories))
	if len(repositories) == 0 || seesEveryObject(reader) {
		return hidden, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, repositoriesHiddenQuery, refs.VisibilityArg(reader), repositories)
	if err != nil {
		return nil, fmt.Errorf("read whether the caller sees %d repositories: %w", len(repositories), err)
	}
	defer rows.Close()
	for rows.Next() {
		var repository string
		var concealed bool
		if err := rows.Scan(&repository, &concealed); err != nil {
			return nil, fmt.Errorf("read whether the caller sees a repository: %w", err)
		}
		hidden[repository] = concealed
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read whether the caller sees %d repositories: %w", len(repositories), err)
	}
	return hidden, nil
}
