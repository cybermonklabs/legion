package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
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
// installation lookup under the App's JWT, its 404 being false. Any authenticated caller may ask:
// the Legion daemon asks it for every repository it serves, since on a repository the App is not
// installed on GitHub sends Legion none of its events and nothing redelivers them. A path that
// names no repository GitHub allows is 400 INVALID_REPOSITORY before GitHub is asked anything
// (githubRepoPath), since its halves go into the lookup's path as they are. Dispatch with no App
// key answers 503 GITHUB_APP_UNCONFIGURED; any other failed lookup 502 GITHUB_UNREADABLE, naming
// the repository and GitHub's error, worded and cut to its bound as every such 502 words it
// (gitHubReason): an error GitHub answered with a page that is no JSON carries the page as
// githubapp.ShownBody shows it, its first 512 characters and its size.
//
// A repository the caller may not see by the GitHub proxy's rule (repositoriesHidden, over
// refs.RepositoryClause) answers exactly what one the App is not installed on answers, as the
// proxy answers it (githubapi.ProxyREST), and GitHub is asked nothing about it: that the App is
// installed on a repository a private project's issues name would tell a caller who does not see
// the project that the repository is served. While a rate limit is in force on the App's scope,
// the lookup of an uninstalled repository is held unsent and answers that limit
// (githubapp.Client.HeldLimit), so a hidden one answers the same limit; otherwise it is not
// installed. No App key answers every repository alike, so it is answered before the repository is
// read. A reader that sees every object, as every open viewer of a deployment with no private
// project does, reads no statement for it and is answered as before any project could be private
// (R9).
func (s *server) getAppInstallation(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, name, err := githubRepoPath(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	repo := owner + "/" + name
	if s.deps.GitHub == nil {
		writeError(w, "GITHUB_APP_UNCONFIGURED", http.StatusServiceUnavailable, githubapp.ErrNoAppKey.Error())
		return
	}
	hidden, err := s.repositoriesHidden(r.Context(), reader, []string{repo})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var installed bool
	if !hidden[repo] {
		// While a project is private the lookup is held to the path's repository, as the GitHub
		// proxy holds its reads (githubapp.HoldToRepository): GitHub answers an old name of a renamed
		// or transferred repository with a redirect to that repository as it is now, which
		// repositoriesHidden never judged, so such a redirect answers as a repository the App is not
		// installed on. With no private project the lookup follows GitHub's redirects, as before.
		ctx := r.Context()
		if reader.Viewer != nil && reader.Viewer.AnyPrivate {
			ctx = githubapp.HoldToRepository(ctx, owner, name)
		}
		installed, err = s.deps.GitHub.Installed(ctx, owner, name)
		if errors.Is(err, githubapp.ErrRedirectedOffRepository) {
			installed, err = false, nil
		}
	} else if limited := s.deps.GitHub.HeldLimit(r.Context(), githubapp.AppScope); limited != nil {
		err = limited
	}
	if err != nil {
		writeError(w, "GITHUB_UNREADABLE", http.StatusBadGateway, fmt.Sprintf("read the GitHub App's installation on %s: %s", repo, gitHubReason(err)))
		return
	}
	WriteJSON(w, http.StatusOK, appInstallationResponse{Installed: installed})
}

// repositoriesHiddenQuery is refs.RepositoryClause over each repository of an array, the reader's
// visibility argument bound at $1 and the repositories at $2, each answered with whether it is
// hidden. Neither placeholder differs between reads, so the text is built once.
var repositoriesHiddenQuery = `select asked.repo, not ` + refs.RepositoryClause("asked.repo", "$1") + ` from unnest($2::text[]) as asked(repo)`

// repositoriesHidden answers, for each of repositories (owner/name), whether reader may not see it:
// whether it fails refs.RepositoryClause, read in one statement and keyed by the name as given. A
// reader that sees every object (seesEveryObject) sees every repository, and nothing is read for
// it (R9); nothing is read for no repository either. The App-installation route, the GitHub proxy
// (githubGate), and delivery's pull requests that name no issue and its runs that ship none
// (deliveryPopulation.cutTo), hold a repository to it.
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

// RepositoryGate is the GitHub proxy's gate for the caller Background.GitHubGate resolved
// (githubGate): for one repository, owner/name as the proxy's path spells it, whether the caller
// may not see it and whether a read of it is held to it (githubapi.Gate's Hidden and Held). An
// error is the proxy's 500.
type RepositoryGate func(repository string) (hidden, held bool, err error)

// githubGate is the GitHub proxy's caller check (Background.GitHubGate), which routes'
// apiGithubRest asks before the proxy reads anything (githubapi.ProxyREST), and the gate it then
// holds each read to. The caller is the request's own credential, resolved as every row of the
// table resolves one (optionalActor): a person's cookie or trusted header, a personal token, or a
// token the secrets broker issued. A caller that does not resolve is refused as every gate refuses
// one (writeAuthenticationError), and a read-only token is 403 READ_ONLY_TOKEN, as every row the
// read list does not name answers it (refuseReadOnlyToken).
//
// Then the caller's viewer is resolved (access.Resolve, its credential's list applied), and a token
// of a restricted member, of any kind, is 403 RESTRICTED_MEMBER, whatever scopes its row or the
// broker names. A restricted member's tokens hold no scope (R16): their browser's mint gives none,
// and personalTokenActor and brokerTokenActor drop any a token was given before that rule. The
// refusal comes before the scope check, so it says why such a token reads nothing rather than
// which scope it lacks. The test is the one every write of the API reads for a restricted member
// (model.Viewer.Restricted, withinProjectScope, refuseRepoOfRestrictedMember), so an outside
// person's agents read nothing through the proxy (R9's exception). A restricted member's browser
// session is held to the repository rule, as before (R2): it reads a repository an issue of a
// project whose list names them resolves to, and every other one answers as one the App is not
// installed on.
//
// Last, a credential that does not hold the person scope is 403 OAUTH_SCOPE_REQUIRED naming it, as
// gateRouteScopes answers: the deployment's shared token, a service account's, a token an
// operator's entry mapped to one project and a machine's that names no person hold none. Each
// refusal comes before the method and the path are read, and GitHub is asked nothing.
//
// The gate is the App-installation route's (repositoriesHidden) for that viewer: hidden is whether
// it fails refs.RepositoryClause, and held is its private flag, the one the clause's R9 arm reads.
// The viewer is resolved without the listener refresh readerFor makes first, since the clause reads
// no session's content. A server with no store has no viewer to resolve: every repository is hidden
// and every read held, as the closed viewer reads.
func (s *server) githubGate(w http.ResponseWriter, r *http.Request) (RepositoryGate, bool) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return nil, false
	}
	if actor.Credential != nil && actor.Credential.ReadOnly {
		writeError(w, contracts.DispatchReadOnlyToken, http.StatusForbidden, s.readOnlyRefusal)
		return nil, false
	}
	stored := s.deps.Store != nil && s.deps.Store.Pool != nil
	var viewer model.Viewer
	if stored {
		if viewer, err = access.Resolve(r.Context(), s.deps.Store.Pool, actor); err != nil {
			s.writeHandlerError(w, fmt.Errorf("resolve what the GitHub proxy's caller sees: %w", err))
			return nil, false
		}
		if viewer.Restricted && !human {
			writeError(w, codeRestrictedMember, http.StatusForbidden,
				"a restricted member's token reads nothing through the GitHub proxy, whatever scopes it holds")
			return nil, false
		}
	}
	if !actor.Credential.Holds(model.ScopePerson) {
		refuseMissingScope(w, model.ScopePerson, fmt.Sprintf(
			"this request's credential does not hold the %s scope, which the GitHub proxy requires", model.ScopePerson))
		return nil, false
	}
	if !stored {
		return func(string) (bool, bool, error) { return true, true, nil }, true
	}
	reader := actor.Reader()
	reader.Viewer = &viewer
	return func(repository string) (bool, bool, error) {
		hidden, err := s.repositoriesHidden(r.Context(), reader, []string{repository})
		if err != nil {
			return false, false, err
		}
		return hidden[repository], viewer.AnyPrivate, nil
	}, true
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
