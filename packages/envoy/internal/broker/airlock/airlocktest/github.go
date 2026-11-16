// packages/envoy/internal/broker/airlock/airlocktest/github.go
// The GitHub stand-in a Service answers as: the App endpoints a narrowed github-app release is
// made and given back over, and the repository read one of its tokens is spent at. It is a
// value holding what it answers and what it was asked, as the seven stand-ins of stands.go are,
// and it installs its Answer on a Service (Service.AnswerWith), so it may play a host of its own
// in the command and it may play the bound host of a row that already has a service. It is in a
// file of its own because it is the longest of the stand-ins by far — four routes, and a record
// of what each token it minted reaches — and a person who changes another stand-in has no
// reason to read it.
package airlocktest

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// "" is GitHubTokensOrdinary. GitHubStand.Answers replaces it while the stand runs, which is
	// how one stack drives all three answers one after another.
	Answer GitHubTokenAnswer
}

// GitHubTokenAnswer is one of the three answers a GitHubStand gives a token request, which
// GitHubStand.Answers sets while the stand runs. The values are the words the answer route of
// cmd/dev-airlock-stands takes in its body, so a check script names the same three answers a Go
// test does.
type GitHubTokenAnswer string

const (
	// GitHubTokensOrdinary answers each token request what its own body asked for, as GitHub does.
	GitHubTokensOrdinary GitHubTokenAnswer = "ordinary"
	// GitHubTokensRefused answers every token request HTTP 422 and mints nothing, which is what
	// GitHub answers a body naming a permission the installation does not hold. The answer says
	// nothing of a rate limit, so the minter reads it as the caller's narrowing and refuses the
	// release (secrets.GitHubNarrowing.refusal) rather than as the upstream's trouble. A
	// read-only narrowing asks once more without actions, which this answers 422 as well, so that
	// refusal reaches the caller too and no token is ever released.
	GitHubTokensRefused GitHubTokenAnswer = "refuse"
	// GitHubTokensWider answers each token request a token wider than its body asked for: every
	// repository of the account rather than the ones the body named, and a grant past the read
	// set a read-only body asks for. GitHub applies the body it is sent, so a token like this is
	// one something between the broker and GitHub widened: the minter revokes it and releases
	// nothing (secrets.GitHubNarrowing.heldBy). It is how a check drives that path, which nothing
	// else reaches.
	GitHubTokensWider GitHubTokenAnswer = "widen"
)

// standInGitHubPermissions is what a token holds when its request named none: the permissions
// this stand-in says every installation of the App was granted. GitHub answers such a request a
// token holding all of the installation's, and the minter checks the permissions of a read-only
// narrowing alone, which always names them, so nothing but a caller reading what it got reads
// these.
var standInGitHubPermissions = map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}

// widerPermission is the grant a widened answer holds past what its body asked for. No narrowing
// asks for it, and the minter refuses a read-only token holding a permission outside contents,
// metadata, pull requests and actions, so it is the permission half of a token wider than asked.
const widerPermission, widerPermissionLevel = "administration", "write"

// GitHubStand is the four routes a narrowed github-app release is made, spent and given back
// over: GET /app/installations, which answers the installations it was given; POST
// /app/installations/{id}/access_tokens, which answers that installation's token, its expiry and
// what the token holds, read from the body the request sent; GET /repos/{owner}/{repo}, the
// read a caller makes with such a token, which answers only for a token this stand-in minted
// whose scope holds that repository; and DELETE /installation/token, the revoke the minter makes
// of a token it minted and will not release. Pointing BROKER_AIRLOCK_TEST_GITHUB_API_BASE at a
// Service answering as one is what makes a github-app secret mint against this process.
//
// It remembers what each token it minted reaches, since that is the whole of what the repository
// route decides on, and one stand-in answers several callers of a stack at once, so that record
// is held under a lock, as the Microsoft stand-in holds its exchanges. The answer its token route
// gives is held under that same lock, since a check sets it while the stand serves (Answers).
type GitHubStand struct {
	options GitHubOptions

	mu     sync.Mutex
	answer GitHubTokenAnswer
	scopes map[string]githubScope
}

// githubScope is what one minted token reaches: the account it was minted for, and the
// repositories of that account it names, where none is every repository the installation reaches.
type githubScope struct {
	owner        string
	repositories []string
}

// NewGitHubStand builds one, answering as its options say to begin with, where an Answer of ""
// is the ordinary mint. Install it with Service.AnswerWith(stand.Answer).
func NewGitHubStand(o GitHubOptions) *GitHubStand {
	answer := o.Answer
	if answer == "" {
		answer = GitHubTokensOrdinary
	}
	return &GitHubStand{options: o, answer: answer, scopes: map[string]githubScope{}}
}

// Answers replaces what every token request from here on is answered with, which is how one run
// of a stack drives the ordinary mint, the refusal and the token wider than asked one after
// another. It mints nothing itself, so a token already minted keeps its string, but it does not
// keep what that string reaches: one string stands for one owner and one repository list
// (token), and every mint of that scope records anew what it reaches (remember), under the
// answer in force at the time. So a widened mint of a scope an ordinary mint already minted
// widens what that one string reaches, and the minter's revoke of the widened token leaves the
// string a credential of nobody's (forget). A check that wants an ordinary token after another
// answer minted its scope mints it again.
func (g *GitHubStand) Answers(answer GitHubTokenAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.answer = answer
}

// answering is what the token route answers now.
func (g *GitHubStand) answering() GitHubTokenAnswer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.answer
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
// answered one string for every scope could not tell a narrowed caller from a wide one at its
// repository route. Two requests naming the same repositories are answered the same token, so
// two runs of a check read one value, and what that token reaches is what the last mint of that
// scope recorded (remember).
func (g *GitHubStand) token(owner string, repositories []string) string {
	token := g.options.Token
	if token == "" {
		token = "ghs_standin_" + owner
	}
	if len(repositories) == 0 {
		return token
	}
	return token + "_" + strings.Join(repositories, "_")
}

// remember records what the token this stand-in has just minted reaches, which is what the
// repository route answers from.
func (g *GitHubStand) remember(token, owner string, repositories []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scopes[token] = githubScope{owner: owner, repositories: slices.Clone(repositories)}
}

// scopeOf is what token reaches, and false for a credential this stand-in never minted.
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

// Answer is GitHub's answer to each of the four routes, and a 404 naming anything else: a caller
// that called a route this stand-in does not serve reads which one rather than an empty answer.
func (g *GitHubStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	w.Header().Set("Content-Type", "application/json")
	switch owner, repository, route := g.route(seen); route {
	case "installations":
		installations := []map[string]any{}
		for _, owner := range g.owners() {
			installations = append(installations, map[string]any{
				"id":      g.options.Installations[owner],
				"account": map[string]string{"login": owner},
			})
		}
		_ = json.NewEncoder(w).Encode(installations)
	case "token":
		g.answerToken(w, seen, owner)
	case "repository":
		g.answerRepository(w, githubCredential(seen), owner, repository)
	case "revoke":
		g.answerRevoke(w, githubCredential(seen))
	default:
		http.Error(w, "the GitHub stand-in serves GET /app/installations, POST /app/installations/{id}/access_tokens, GET /repos/{owner}/{repo} and DELETE /installation/token, not "+seen.Method+" "+seen.Path, http.StatusNotFound)
	}
	return true
}

// answerToken is GitHub's answer to one access_tokens request: the token, its expiry and what
// GitHub says the token holds, which is what the minter checks a narrowing against
// (secrets.GitHubNarrowing.heldBy). The answer echoes the body: a selection of the repositories
// it named, or all of the account's when it named none, and the permissions it asked for. The
// other two answers (Answers) answer in its place, one refusing the mint and one minting wider
// than asked.
func (g *GitHubStand) answerToken(w http.ResponseWriter, seen Request, owner string) {
	answering := g.answering()
	if answering == GitHubTokensRefused {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":           "The permissions requested are not granted to this installation.",
			"documentation_url": "https://docs.github.com/rest/apps/apps#create-an-installation-access-token-for-an-app",
			"status":            "422",
		})
		return
	}
	asked := githubTokenRequest(seen)
	token := g.token(owner, asked.Repositories)
	selection, repositories := "selected", asked.Repositories
	if len(repositories) == 0 {
		selection = "all"
	}
	permissions := asked.Permissions
	if len(permissions) == 0 {
		permissions = standInGitHubPermissions
	}
	if answering == GitHubTokensWider {
		// Wider on both of the rules a narrowed token is checked by: every repository of the
		// account, and a grant the body did not ask for.
		selection, repositories = "all", nil
		wider := maps.Clone(permissions)
		wider[widerPermission] = widerPermissionLevel
		permissions = wider
	}
	g.remember(token, owner, repositories)
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
	// A scope that names no repository reaches every repository of its account, which is the
	// token a narrowing that named none is minted as.
	reaches := len(scope.repositories) == 0 ||
		slices.ContainsFunc(scope.repositories, func(name string) bool { return strings.EqualFold(name, repository) })
	if !strings.EqualFold(scope.owner, owner) || !reaches {
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
// that sent no body, which is a mint narrowing nothing, and for a body that is no such object.
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

// route reads which of the four routes a request is: the installations list; a token request,
// with the owner whose installation id it names; a repository read, with the account and the
// repository it names; and the revoke of a token, which names nothing but itself, in its
// Authorization header. An installation id no owner holds is no route of this stand-in's: a
// minter that asked for a token of an installation nothing was configured for reads the 404
// above rather than a token of nobody's.
func (g *GitHubStand) route(seen Request) (owner, repository, route string) {
	if seen.Method == http.MethodGet && seen.Path == "/app/installations" {
		return "", "", "installations"
	}
	if seen.Method == http.MethodDelete && seen.Path == "/installation/token" {
		return "", "", "revoke"
	}
	if rest, isRepository := strings.CutPrefix(seen.Path, "/repos/"); isRepository && seen.Method == http.MethodGet {
		account, name, named := strings.Cut(rest, "/")
		if named && account != "" && name != "" && !strings.Contains(name, "/") {
			return account, name, "repository"
		}
	}
	rest, isToken := strings.CutSuffix(strings.TrimPrefix(seen.Path, "/app/installations/"), "/access_tokens")
	if seen.Method != http.MethodPost || !isToken {
		return "", "", ""
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", "", ""
	}
	for _, owner := range g.owners() {
		if g.options.Installations[owner] == id {
			return owner, "", "token"
		}
	}
	return "", "", ""
}
