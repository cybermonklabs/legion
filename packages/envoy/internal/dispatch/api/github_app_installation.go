package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// appInstallationResponse is GET /api/v1/github/app-installation/{owner}/{repo}'s answer.
type appInstallationResponse struct {
	Installed bool `json:"installed"`
}

// getAppInstallation answers whether Dispatch's GitHub App, the one GitHub sends the webhook
// events of its repositories through, is installed on the path's repository: GitHub's
// installation lookup under the App's JWT, its 404 being false. The Legion daemon asks it for
// every repository it serves, since on a repository the App is not installed on GitHub sends
// Legion none of its events and nothing redelivers them. Dispatch with no App key answers 503
// GITHUB_APP_UNCONFIGURED; any other failed lookup 502 GITHUB_UNREADABLE, naming the repository
// and GitHub's error.
//
// A repository the caller may not see (access.RepositoryClause: one mapped to no project it sees,
// or one no project maps while it is not an open viewer) answers exactly what one the App is not
// installed on answers, as the GitHub proxy's gate does (githubapi.ProxyREST). The lookup is made
// for it as for any repository, so the two answers shared by every repository stay as they are -
// no App key, and a rate limit met on the App's scope - and every other outcome, the App being
// installed included, is GitHub's 404: installed false.
func (s *server) getAppInstallation(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	repo, err := repoProjectPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	owner, name, _ := strings.Cut(repo, "/")
	installed, err := s.deps.GitHub.Installed(r.Context(), owner, name)
	if _, limited := githubapp.AsRateLimit(err); !limited && !errors.Is(err, githubapp.ErrNoAppKey) {
		if _, poolErr := s.readyPool(); poolErr != nil {
			s.writeHandlerError(w, poolErr)
			return
		}
		visible, seenErr := s.visibleRepositories(r.Context(), refs.VisibilityArg(reader), []string{repo})
		if seenErr != nil {
			s.writeHandlerError(w, seenErr)
			return
		}
		if !visible[repo] {
			installed, err = false, nil
		}
	}
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		writeError(w, "GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, "GITHUB_UNREADABLE", http.StatusBadGateway, fmt.Sprintf("read the GitHub App's installation on %s: %v", repo, err))
	default:
		WriteJSON(w, http.StatusOK, appInstallationResponse{Installed: installed})
	}
}
