package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// appInstallationResponse is GET /api/v1/github/app-installation/{owner}/{repo}'s answer.
type appInstallationResponse struct {
	Installed bool `json:"installed"`
}

// getAppInstallation answers whether Dispatch's GitHub App, the one GitHub sends the webhook
// events of its repositories through, is installed on the path's repository: GitHub's
// installation lookup under the App's JWT, its 404 being false. Any authenticated caller may ask:
// the Legion daemon asks it for every repository it serves, since on a repository the App is not
// installed on GitHub sends Legion none of its events and nothing redelivers them. A path that
// names no repository GitHub allows is 400 INVALID_REPOSITORY before GitHub is asked anything
// (githubRepoPath), since its halves go into the lookup's path as they are. Dispatch with no App
// key answers 503 GITHUB_APP_UNCONFIGURED; any other failed lookup 502 GITHUB_UNREADABLE, naming
// the repository and GitHub's error, worded and cut to its bound as every such 502 words it
// (gitHubReason): an error GitHub answered with a page that is no JSON carries the page as
// githubapp.ShownBody shows it, its first 512 characters and its size.
func (s *server) getAppInstallation(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, name, err := githubRepoPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	installed, err := s.deps.GitHub.Installed(r.Context(), owner, name)
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		writeError(w, "GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeError(w, "GITHUB_UNREADABLE", http.StatusBadGateway, fmt.Sprintf("read the GitHub App's installation on %s/%s: %s", owner, name, gitHubReason(err)))
	default:
		WriteJSON(w, http.StatusOK, appInstallationResponse{Installed: installed})
	}
}

// githubRepoPath reads the {owner}/{repo} path of the App's installation read as parseSourceRepo
// reads a repository: an owner and a name of the characters GitHub allows, and never a name of
// `.` or `..`, before or after a trailing `.git` is trimmed, canonicalised, held to that rule as
// it is stored, and answered as its two halves. A path the rule refuses is 400
// INVALID_REPOSITORY. The read puts the halves into GitHub's installation path at once, and
// `...git` and `.git` are no dot segments, so the mux does not clean them, while the trailing
// `.git` refs.ExternalRepo trims would leave `..` or an empty name behind.
func githubRepoPath(r *http.Request) (owner, name string, err error) {
	owner, name, err = parseSourceRepo(r.PathValue("owner") + "/" + r.PathValue("repo"))
	if err != nil {
		return "", "", errorf(http.StatusBadRequest, "INVALID_REPOSITORY", `the path must name a repository "owner/name" (got %q)`,
			r.PathValue("owner")+"/"+r.PathValue("repo"))
	}
	return owner, name, nil
}
