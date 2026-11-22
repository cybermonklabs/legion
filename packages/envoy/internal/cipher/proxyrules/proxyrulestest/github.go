// packages/envoy/internal/cipher/proxyrules/proxyrulestest/github.go
// The GitHub stand-in a Service answers as: the App endpoints a github-app profile's release is
// made and given back over, the repository read one of its tokens is spent at, and the three
// routes that answer by the permissions a token holds. It is a value holding what it answers and
// what it was asked, as the seven stand-ins of stands.go are, and it installs its Answer on a
// Service (Service.AnswerWith), so it may play a host of its own in the command and it may play
// the bound host of a row that already has a service. It is in a file of its own because it is
// the longest of the stand-ins by far — seven routes, and a record of what each token it minted
// reaches and holds — and a person who changes another stand-in has no reason to read it.
package proxyrulestest

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// GitHubOptions is what a GitHubStand answers with.
type GitHubOptions struct {
	// Installations is the installation id the App has on each owner, by owner. A mint for an
	// owner no entry names finds no installation, which is what the minter's own refusal is
	// about.
	Installations map[string]int64
	// Token is the installation token every answer carries, and "" is a token of each owner's
	// own, ghs_standin_<owner>, so a row that mints for two owners can tell the two apart. A
	// request that named repositories is answered that token and the names it asked for, since
	// the repository route below tells one scope from another by the token string alone.
	Token string
	// Lifetime is how long from now each token's expires_at is, and zero is an hour, the life
	// GitHub gives one.
	Lifetime time.Duration
	// Answer is the answer the stand starts on, for a Go test that sets it at construction, and
	// the zero value is the ordinary mint. GitHubStand.Answers replaces it while the stand runs,
	// which is how one stack drives the answers one after another.
	Answer GitHubAnswer
	// DistinctTokens has every token request minted a token of its own, as GitHub mints one, by
	// adding the request's number to the string token would answer. The default answers one
	// string for each scope, so that two runs of a check read one value; a test that counts the
	// tokens left live, one for each request GitHub served, sets it.
	DistinctTokens bool
	// Permissions is what the App's installation on each owner holds, by owner: each permission
	// and its level. The installations listing names it, and the token route refuses a request
	// naming a permission the owner's map does not hold at that level or above, as GitHub does.
	// An owner it has no entry for, and every owner while it is nil, holds
	// standInGitHubPermissions. Every installation GitHub lists holds metadata at read, so an
	// installation of the shape GitHub lists names it: one that holds nothing else is how a test
	// drives the empty cut (secrets' emptyCut).
	Permissions map[string]map[string]string
}

// GitHubAnswer is what a GitHubStand answers each token request with: the mode, and the one
// value a mode reads. It is a value rather than a word alone because two of the modes refuse one
// part of a request and mint every other, and the part they refuse has to be named. Its zero
// value is the ordinary mint.
type GitHubAnswer struct {
	// Mode is which of the answers below each token request gets, and "" is GitHubTokensOrdinary.
	Mode GitHubTokenAnswer
	// Repository is the repository GitHubTokensRefusedRepository refuses, read without regard to
	// case, as GitHub reads a repository name. Every other mode reads none.
	Repository string
}

// GitHubTokenAnswer is one of the six answers a GitHubStand gives a token request, which
// GitHubStand.Answers sets while the stand runs. The values are the words the answer route of
// cmd/dev-proxy-stands takes in its body, so a check script names the same six answers a Go
// test does. An account the App has no installation on is no answer of its own: leaving that
// account out of GitHubOptions.Installations is already that case.
type GitHubTokenAnswer string

const (
	// GitHubTokensOrdinary answers each token request what its own body asked for, with metadata
	// at read beside it, as GitHub does.
	GitHubTokensOrdinary GitHubTokenAnswer = "ordinary"
	// GitHubTokensRefused answers every token request HTTP 422 and mints nothing, which is what
	// GitHub answers a body naming a permission the installation does not hold. The answer says
	// nothing of a rate limit, so the minter reads it as the caller's scope and refuses the
	// release (secrets' refusal) rather than as the upstream's trouble, and no token is ever
	// released.
	GitHubTokensRefused GitHubTokenAnswer = "refuse"
	// GitHubTokensRefusedRepository answers that same 422 to a token request whose repositories
	// name the answer's Repository, and answers every other token request ordinarily. So a scope
	// naming that repository is refused while one naming another is minted, and a request that
	// leaves the repositories out is minted too: that is the reading a caller asking GitHub again
	// with fewer repositories makes to find which one was refused.
	GitHubTokensRefusedRepository GitHubTokenAnswer = "refuse-repository"
	// GitHubTokensRefusedPermissions answers that 422 to a token request whose body names any
	// permissions, which every profile's token request does (secrets' cut), and answers a request
	// that names none ordinarily. A per-owner refusal of the permissions alone is
	// GitHubOptions.Permissions instead, which refuses what that owner does not hold.
	GitHubTokensRefusedPermissions GitHubTokenAnswer = "refuse-permissions"
	// GitHubTokensWider answers each token request a token wider than its body asked for: every
	// repository of the account rather than the ones the body named, and a grant the body did not
	// name. GitHub applies the body it is sent, so a token like this is one something between the
	// broker and GitHub widened: the minter revokes it and releases nothing (secrets' heldBy). It
	// is how a check drives that path, which nothing else reaches.
	GitHubTokensWider GitHubTokenAnswer = "widen"
	// GitHubTokensFailOnce answers the next token request HTTP 503, as a GitHub outage does, and
	// mints nothing for it, and then answers as GitHubTokensOrdinary from the request after it on.
	// The minter retries a 503 (secrets.Retry), so a release against it mints at its second
	// attempt: it is how a check drives the retry, which nothing else reaches on a stack.
	GitHubTokensFailOnce GitHubTokenAnswer = "fail-once"
)

// refuses reports whether this answer refuses the token request that asked: every one of them
// for GitHubTokensRefused; one whose repositories name the answer's Repository for
// GitHubTokensRefusedRepository; and one whose body names any permissions for
// GitHubTokensRefusedPermissions. Every other answer refuses nothing and mints below.
func (a GitHubAnswer) refuses(asked githubAccessTokens) bool {
	switch a.Mode {
	case GitHubTokensRefused:
		return true
	case GitHubTokensRefusedRepository:
		return slices.ContainsFunc(asked.Repositories, func(name string) bool {
			return strings.EqualFold(name, a.Repository)
		})
	case GitHubTokensRefusedPermissions:
		return len(asked.Permissions) > 0
	default:
		return false
	}
}

// standInGitHubPermissions is what this stand-in says an installation of the App holds where
// GitHubOptions.Permissions names none for its owner: what the installations listing names, and
// what a token holds when its request named none. A profile's permissions are cut to it (secrets'
// cut), so a profile asking for a permission it does not name, or for one at a level past it,
// has that entry left out, and the release says so.
var standInGitHubPermissions = map[string]string{
	"actions": "read", "checks": "read", "contents": "write", "metadata": "read", "pull_requests": "write", "statuses": "read",
}

// widerPermission is the grant a widened answer holds past what its body asked for. No stand-in
// profile asks for it, and the minter refuses a token holding a permission its request did not
// name, so it is the permission half of a token wider than asked.
const widerPermission, widerPermissionLevel = "administration", "write"

// GitHubCommitSHA is the one commit every repository of the GitHub stand holds: a commit status
// for it is written, and one for any other commit is refused 422, as GitHub refuses a status for
// a commit the repository does not hold. cmd/dev-proxy-stands' report names it
// (github_commit_sha), so a check script writes a status a caller can be granted.
const GitHubCommitSHA = "5fd1c0a5a4e3b2c1d0e9f8a7b6c5d4e3f2a1b0c9"

// GitHubStand is the seven routes a github-app profile's release is made, spent and given back
// over: GET /app/installations, which answers the installations it was given; POST
// /app/installations/{id}/access_tokens, which answers that installation's token, its expiry and
// what the token holds, read from the body the request sent; GET /repos/{owner}/{repo}, the
// read a caller makes with such a token, which answers only for a token this stand-in minted
// whose scope holds that repository; DELETE /installation/token, the revoke the minter makes
// of a token it minted and will not release; and the three routes a check reads what a token
// holds at (answerHeld): GET /repos/{owner}/{repo}/keys, GET /orgs/{org}/installations and POST
// /repos/{owner}/{repo}/statuses/{sha}. Pointing BROKER_AIRLOCK_TEST_GITHUB_API_BASE at a
// Service answering as one is what makes a github-app secret mint against this process.
//
// It remembers what each token it minted reaches and holds, since that is the whole of what the
// repository route and the three others decide on, and one stand-in answers several callers of a
// stack at once, so that record is held under a lock, as the Microsoft stand-in holds its
// exchanges. The answer its token route gives is held under that same lock, since a check sets
// it while the stand serves (Answers).
type GitHubStand struct {
	options GitHubOptions

	mu     sync.Mutex
	answer GitHubAnswer
	scopes map[string]githubScope
	// minted counts the token requests minted, for DistinctTokens.
	minted int
}

// githubScope is what one minted token reaches and holds: the account it was minted for, the
// repositories of that account it names, where none is every repository the installation
// reaches, and the permissions the token route answered it holds, each at its level.
type githubScope struct {
	owner        string
	repositories []string
	permissions  map[string]string
}

// reaches reports whether the token reaches repository of owner, or owner's account itself where
// repository is "", without regard to case, as GitHub compares logins and names. A scope that
// names no repository reaches every repository of its account, which is the token a scope that
// named none is minted as.
func (s githubScope) reaches(owner, repository string) bool {
	if !strings.EqualFold(s.owner, owner) {
		return false
	}
	return repository == "" || len(s.repositories) == 0 ||
		slices.ContainsFunc(s.repositories, func(name string) bool { return strings.EqualFold(name, repository) })
}

// NewGitHubStand builds one, answering as its options say to begin with, where the zero Answer
// is the ordinary mint. Install it with Service.AnswerWith(stand.Answer).
func NewGitHubStand(o GitHubOptions) *GitHubStand {
	answer := o.Answer
	if answer.Mode == "" {
		answer.Mode = GitHubTokensOrdinary
	}
	return &GitHubStand{options: o, answer: answer, scopes: map[string]githubScope{}}
}

// held is what the App's installation on owner holds: its entry of GitHubOptions.Permissions, or
// standInGitHubPermissions where there is none.
func (g *GitHubStand) held(owner string) map[string]string {
	if held, set := g.options.Permissions[owner]; set {
		return held
	}
	return standInGitHubPermissions
}

// holdsAll reports whether the App's installation on owner holds every permission asked names,
// each at the level asked or above, and whether each is asked at a level GitHub gives it: GitHub
// answers 422 to a request past what the installation holds, and to workflows or profile at read.
func (g *GitHubStand) holdsAll(owner string, asked map[string]string) bool {
	held := g.held(owner)
	for permission, level := range asked {
		if githubname.LevelRank(level) == 0 || githubname.LevelRank(held[permission]) < githubname.LevelRank(level) {
			return false
		}
		if level == "read" && githubname.WriteOnlyPermission(permission) {
			return false
		}
	}
	return true
}

// Answers replaces what the token requests from here on are answered with - every one of them,
// but for GitHubTokensFailOnce, which answers the next one alone and then the ordinary mint -
// which is how one run of a stack drives the ordinary mint, each refusal, the token wider than
// asked and the one outage a retry rides out, one after another. It mints nothing itself, so a
// token already minted keeps its string, but it does not keep what that string reaches: one
// string stands for one owner and one repository list (token), and every mint of that scope
// records anew what it reaches (remember), under the answer in force at the time. So a widened
// mint of a scope an ordinary mint already minted widens what that one string reaches, and the
// minter's revoke of the widened token leaves the string a credential of nobody's (forget). A
// check that wants an ordinary token after another answer minted its scope mints it again.
func (g *GitHubStand) Answers(answer GitHubAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// answering is what the token route answers the request it is asked for now. GitHubTokensFailOnce
// is spent by the one request it answers, and the stand answers GitHubTokensOrdinary from then on.
func (g *GitHubStand) answering() GitHubAnswer {
	g.mu.Lock()
	defer g.mu.Unlock()
	answer := g.answer
	if answer.Mode == GitHubTokensFailOnce {
		g.answer = GitHubAnswer{Mode: GitHubTokensOrdinary}
	}
	return answer
}

// owners names every owner the App is installed on, in order, so two runs answer one list.
func (g *GitHubStand) owners() []string {
	owners := make([]string, 0, len(g.options.Installations))
	for owner := range g.options.Installations {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	return owners
}

// token is the installation token this stand-in answers for owner, and a token of its own for a
// request that named repositories: GitHub mints a token for each request, and a stand-in that
// answered one string for every scope could not tell a caller of named repositories from a wide
// one at its repository route. Two requests naming the same repositories are answered the same
// token, so two runs of a check read one value, and what that token reaches is what the last mint
// of that scope recorded (remember) - unless DistinctTokens is set, which numbers each request's
// token.
func (g *GitHubStand) token(owner string, repositories []string) string {
	token := g.options.Token
	if token == "" {
		token = "ghs_standin_" + owner
	}
	if len(repositories) > 0 {
		token += "_" + strings.Join(repositories, "_")
	}
	if g.options.DistinctTokens {
		g.mu.Lock()
		g.minted++
		token += "_" + strconv.Itoa(g.minted)
		g.mu.Unlock()
	}
	return token
}

// remember records what the token this stand-in has just minted reaches and holds, which is what
// the repository route and the three routes that read what a token holds answer from.
func (g *GitHubStand) remember(token, owner string, repositories []string, permissions map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scopes[token] = githubScope{owner: owner, repositories: slices.Clone(repositories), permissions: maps.Clone(permissions)}
}

// scopeOf is what token reaches and holds, and false for a credential this stand-in never minted.
func (g *GitHubStand) scopeOf(token string) (githubScope, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	scope, minted := g.scopes[token]
	return scope, minted
}

// forget drops what token reaches, for the revoke route, and reports whether this stand-in had
// minted it. A token it forgot is a credential of nobody's again, so the repository route reads
// it as it reads one this stand-in never minted.
func (g *GitHubStand) forget(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, minted := g.scopes[token]
	delete(g.scopes, token)
	return minted
}

// Answer is GitHub's answer to each of the seven routes, and a 404 naming anything else: a caller
// that called a route this stand-in does not serve reads which one rather than an empty answer.
func (g *GitHubStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	w.Header().Set("Content-Type", "application/json")
	switch route := g.route(seen); route.name {
	case "installations":
		installations := []map[string]any{}
		for _, owner := range g.owners() {
			installations = append(installations, map[string]any{
				"id":          g.options.Installations[owner],
				"account":     map[string]string{"login": owner},
				"permissions": g.held(owner),
			})
		}
		_ = json.NewEncoder(w).Encode(installations)
	case "token":
		g.answerToken(w, seen, route.owner)
	case "repository":
		g.answerRepository(w, githubCredential(seen), route.owner, route.repository)
	case "revoke":
		g.answerRevoke(w, githubCredential(seen))
	case "keys", "org-installations", "statuses":
		g.answerHeld(w, seen, route)
	default:
		http.Error(w, "the GitHub stand-in serves GET /app/installations, POST /app/installations/{id}/access_tokens, GET /repos/{owner}/{repo}, DELETE /installation/token, GET /repos/{owner}/{repo}/keys, GET /orgs/{org}/installations and POST /repos/{owner}/{repo}/statuses/{sha}, not "+seen.Method+" "+seen.Path, http.StatusNotFound)
	}
	return true
}

// answerToken is GitHub's answer to one access_tokens request: the token, its expiry and what
// GitHub says the token holds, which is what the minter checks its request against (secrets'
// heldBy). A request naming a permission the owner's installation does not hold at that level,
// or workflows or profile at read, is answered 422 and minted nothing, as GitHub answers it
// (holdsAll). The answer echoes the body: a selection of the repositories it named, or all of the
// account's when it named none, and the permissions it asked for, or all the installation holds
// when it named none, with metadata at read beside them wherever they do not name it
// (githubname.MetadataPermission). The other answers (Answers) answer in its place: three that
// refuse this request or every one, one that mints wider than asked, and one outage before it.
func (g *GitHubStand) answerToken(w http.ResponseWriter, seen Request, owner string) {
	answering := g.answering()
	asked := githubTokenRequest(seen)
	if answering.Mode == GitHubTokensFailOnce {
		// GitHub's own 503, which mints nothing. The minter retries it (secrets.Retry), and
		// answering has already put the stand back to the ordinary mint, so the retry is minted.
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Service Unavailable", "status": "503"})
		return
	}
	if answering.refuses(asked) || !g.holdsAll(owner, asked.Permissions) {
		// GitHub's own 422, which mints nothing. One body serves whichever part the answer
		// refuses: the minter reads the status alone and quotes none of GitHub's text (secrets'
		// refusal), which it writes from the request it sent.
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":           "The permissions requested are not granted to this installation.",
			"documentation_url": "https://docs.github.com/rest/apps/apps#create-an-installation-access-token-for-an-app",
			"status":            "422",
		})
		return
	}
	token := g.token(owner, asked.Repositories)
	selection, repositories := "selected", asked.Repositories
	if len(repositories) == 0 {
		selection = "all"
	}
	permissions := maps.Clone(asked.Permissions)
	if len(permissions) == 0 {
		permissions = maps.Clone(g.held(owner))
	}
	if permissions == nil {
		permissions = map[string]string{}
	}
	if _, named := permissions[githubname.MetadataPermission]; !named {
		permissions[githubname.MetadataPermission] = "read"
	}
	if answering.Mode == GitHubTokensWider {
		// Wider on both of the rules a token is checked by: every repository of the account, and
		// a grant the body did not ask for.
		selection, repositories = "all", nil
		wider := maps.Clone(permissions)
		wider[widerPermission] = widerPermissionLevel
		permissions = wider
	}
	g.remember(token, owner, repositories, permissions)
	lifetime := g.options.Lifetime
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	answer := map[string]any{
		"token":                token,
		"expires_at":           time.Now().Add(lifetime).UTC().Format(time.RFC3339),
		"permissions":          permissions,
		"repository_selection": selection,
	}
	if len(repositories) > 0 {
		named := make([]map[string]string, 0, len(repositories))
		for _, repository := range repositories {
			named = append(named, map[string]string{"name": repository, "full_name": owner + "/" + repository})
		}
		answer["repositories"] = named
	}
	// 201 and an expires_at: the minter refuses any other status and refuses a token with no
	// expiry, so an answer short of both is no answer at all.
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(answer)
}

// answerRepository is the read a caller makes with a token this stand-in minted: the repository
// for a token whose scope holds it; 404 for a repository of that same account outside the scope
// and for a repository of any other account, which is how GitHub answers a repository a token
// cannot see; and 401 for a credential this stand-in never minted.
func (g *GitHubStand) answerRepository(w http.ResponseWriter, credential, owner, repository string) {
	scope, minted := g.scopeOf(credential)
	if !minted {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
		return
	}
	if !scope.reaches(owner, repository) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":      repository,
		"full_name": owner + "/" + repository,
		"owner":     map[string]string{"login": owner},
		"private":   true,
	})
}

// answerHeld answers one of the three routes that read what a token holds, as GitHub does: 401
// to a credential this stand-in never minted or one given back; 404 to a token of another
// account, or of other repositories, since GitHub shows a token nothing it cannot reach; 403 to
// a token that does not hold the route's permission at the level it needs, naming both; and then
// the route's own answer: an empty list of deploy keys, the App's installations on the
// organization, and a commit status written, or 422 for a commit other than GitHubCommitSHA, as
// GitHub refuses a status for a commit the repository does not hold.
func (g *GitHubStand) answerHeld(w http.ResponseWriter, seen Request, route githubRoute) {
	scope, minted := g.scopeOf(githubCredential(seen))
	switch {
	case !minted:
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
		return
	case !scope.reaches(route.owner, route.repository):
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		return
	case githubname.LevelRank(scope.permissions[route.permission]) < githubname.LevelRank(route.level):
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Resource not accessible by integration",
			"needs":   route.permission + ":" + route.level,
		})
		return
	}
	switch route.name {
	case "keys":
		_ = json.NewEncoder(w).Encode([]any{})
	case "org-installations":
		installations := []map[string]any{}
		for _, owner := range g.owners() {
			if strings.EqualFold(owner, route.owner) {
				installations = append(installations, map[string]any{"id": g.options.Installations[owner], "account": map[string]string{"login": owner}})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(installations), "installations": installations})
	case "statuses":
		if route.sha != GitHubCommitSHA {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "No commit found for SHA: " + route.sha})
			return
		}
		var asked struct {
			State   string `json:"state"`
			Context string `json:"context"`
		}
		_ = json.Unmarshal([]byte(seen.Body), &asked)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"state": asked.State, "context": asked.Context, "sha": route.sha})
	}
}

// answerRevoke is GitHub's answer to the revoke a caller makes with the token itself, which the
// minter makes of a token it minted and will not release
// (secrets.GitHubApp.revokeInstallationToken): 204 and no body for a token this stand-in minted,
// which it then forgets, so a later read with that token is refused as any other credential of
// nobody's is; and 401 for a credential it never minted, as the repository route answers one.
// GitHub's own 204 carries no body, so the content type Answer set is dropped again.
func (g *GitHubStand) answerRevoke(w http.ResponseWriter, credential string) {
	if !g.forget(credential) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
		return
	}
	w.Header().Del("Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// githubAccessTokens is the access_tokens body a token request sent, the fields the minter writes
// into one (secrets' githubAccessTokensBody).
type githubAccessTokens struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

// githubTokenRequest is that body as the request sent it, and both fields empty for a request
// that sent no body, which names nothing, and for a body that is no such object.
func githubTokenRequest(seen Request) githubAccessTokens {
	var asked githubAccessTokens
	if strings.TrimSpace(seen.Body) == "" {
		return asked
	}
	_ = json.Unmarshal([]byte(seen.Body), &asked)
	return asked
}

// githubCredential is the credential a request presented, read from the Authorization header
// GitHub takes one in: "Bearer <token>", which the minter and a curl of the API both send, and
// "token <token>", which GitHub takes as well. The scheme is read without regard to case, as
// GitHub reads it. It is "" for a request that presented none, which is no token of this
// stand-in's, so such a request is refused with every other unminted one.
func githubCredential(seen Request) string {
	header := seen.Header.Get("Authorization")
	for _, scheme := range []string{"Bearer ", "token "} {
		if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
			return strings.TrimSpace(header[len(scheme):])
		}
	}
	return ""
}

// githubRoute is which of the seven routes a request is, by name, "" for none, with what its path
// names: the owner whose installation a token request names, or the account a repository read or
// a route that reads what a token holds names; the repository, where the path names one; and the
// commit a status is for. Permission and level are what a token needs to be answered at a route
// that reads what it holds, and "" at the other four.
type githubRoute struct {
	name                   string
	owner, repository, sha string
	permission, level      string
}

// route reads which of the seven routes a request is: the installations list; a token request,
// with the owner whose installation id it names; a repository read, with the account and the
// repository it names; the revoke of a token, which names nothing but itself, in its
// Authorization header; and the three routes that read what a token holds (heldRoute). An
// installation id no owner holds is no route of this stand-in's: a minter that asked for a token
// of an installation nothing was configured for reads the 404 above rather than a token of
// nobody's.
func (g *GitHubStand) route(seen Request) githubRoute {
	if seen.Method == http.MethodGet && seen.Path == "/app/installations" {
		return githubRoute{name: "installations"}
	}
	if seen.Method == http.MethodDelete && seen.Path == "/installation/token" {
		return githubRoute{name: "revoke"}
	}
	if held, ok := heldRoute(seen); ok {
		return held
	}
	if rest, isRepository := strings.CutPrefix(seen.Path, "/repos/"); isRepository && seen.Method == http.MethodGet {
		account, name, named := strings.Cut(rest, "/")
		if named && account != "" && name != "" && !strings.Contains(name, "/") {
			return githubRoute{name: "repository", owner: account, repository: name}
		}
	}
	rest, isToken := strings.CutSuffix(strings.TrimPrefix(seen.Path, "/app/installations/"), "/access_tokens")
	if seen.Method != http.MethodPost || !isToken {
		return githubRoute{}
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return githubRoute{}
	}
	for _, owner := range g.owners() {
		if g.options.Installations[owner] == id {
			return githubRoute{name: "token", owner: owner}
		}
	}
	return githubRoute{}
}

// heldRoute reads which of the three routes that read what a token holds a request is, and false
// for every other request: GET /repos/{owner}/{repo}/keys, a repository's deploy keys, which
// needs administration; GET /orgs/{org}/installations, an organization's installations, which
// needs organization_administration; and POST /repos/{owner}/{repo}/statuses/{sha}, a commit
// status, which needs statuses at write. These are the permissions of GitHub's own REST
// description for each route.
func heldRoute(seen Request) (githubRoute, bool) {
	parts := strings.Split(strings.TrimPrefix(seen.Path, "/"), "/")
	switch {
	case seen.Method == http.MethodGet && len(parts) == 4 && parts[0] == "repos" && parts[3] == "keys" && parts[1] != "" && parts[2] != "":
		return githubRoute{name: "keys", owner: parts[1], repository: parts[2], permission: "administration", level: "read"}, true
	case seen.Method == http.MethodGet && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "installations" && parts[1] != "":
		return githubRoute{name: "org-installations", owner: parts[1], permission: "organization_administration", level: "read"}, true
	case seen.Method == http.MethodPost && len(parts) == 5 && parts[0] == "repos" && parts[3] == "statuses" && parts[1] != "" && parts[2] != "" && parts[4] != "":
		return githubRoute{name: "statuses", owner: parts[1], repository: parts[2], sha: parts[4], permission: "statuses", level: "write"}, true
	}
	return githubRoute{}, false
}
