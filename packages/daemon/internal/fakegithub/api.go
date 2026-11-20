package fakegithub

import (
	"cmp"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The REST and GraphQL API, as the daemon, its pods and the leak guard call it (the calls are
// listed where each is answered below):
//
//   - an App's installation tokens, key mode (internal/appauth): GET /app/installations, GET
//     /app/installations/{id}, POST /app/installations/{id}/access_tokens, GET /app, GET
//     /repos/{owner}/{repo}/installation and GET /users/{login}, each but the last under the App's
//     JWT, which the server verifies against the App's key;
//   - token mode's reads of a token (appauth's tokencommand.go): GraphQL `{viewer{login}}`, GET
//     /apps/{slug} and GET /installation/repositories;
//   - a repository's name, default branch and visibility: GET /repos/{owner}/{repo}
//     (ghbranch.Read, the leak guard's REST visibility read), and the leak guard's GraphQL
//     visibility read (leakguard.VisibilityGraphQLArgs);
//   - branches: GET /repos/{owner}/{repo}/git/ref/heads/{branch} and POST
//     /repos/{owner}/{repo}/git/refs (ghbranch.Create), read from and written to the bare
//     repository git serves;
//   - pull requests: GET /repos/{owner}/{repo}/pulls?head=&state= (githubrest.OpenPullRequestOn and
//     the daemon's pull request reads), GET .../pulls/{number}, GET .../pulls/{number}/reviews and
//     GET .../pulls/{number}/reviews/{id};
//   - GET .../collaborators/{login}/permission and GET .../activity?ref= (the review round's reads);
//   - GET .../tarball/{ref} (the leak check's source), a gzipped tar git archive writes.
//
// Anything else answers 404 with GitHub's Not Found body, so a call this list lacks fails loudly
// in the test that makes it.

// grantedPermissions is what every installation of AddApp's is granted: everything the daemon's
// two Apps ask for (appauth's writePermissions and readPermissions), at the most either asks.
var grantedPermissions = map[string]string{
	"actions": "write", "attestations": "write", "checks": "write", "contents": "write", "issues": "write",
	"metadata": "read", "pull_requests": "write", "security_events": "write", "statuses": "write", "workflows": "write",
}

// access orders GitHub's permission levels.
var access = map[string]int{"read": 1, "write": 2, "admin": 3}

// App is a GitHub App AddApp made: its id, its slug, whose bot login is "<slug>[bot]", and its
// private key, PEM, which signs the JWT its installation tokens are minted under.
type App struct {
	ID         int64
	Slug       string
	PrivateKey string
}

type app struct {
	App
	key   *rsa.PublicKey
	botID int64
	// installations is each owner's installation id, by the owner lowercased; owners is each
	// installation's owner as written.
	installations map[string]int64
	owners        map[int64]string
}

// AddApp makes an App of slug installed on each of owners, every installation granted everything
// the daemon's Apps ask for.
func (s *Server) AddApp(t testing.TB, slug string, owners ...string) App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	made := &app{
		App: App{ID: s.nextID(), Slug: slug,
			PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))},
		key: &key.PublicKey, botID: s.nextID(), installations: map[string]int64{}, owners: map[int64]string{},
	}
	for _, owner := range owners {
		id := s.nextID()
		made.installations[strings.ToLower(owner)] = id
		made.owners[id] = owner
	}
	s.apps[made.ID] = made
	return made.App
}

// PullRequest is a pull request of a served repository, as the server answers it.
type PullRequest struct {
	Number      int
	Title, Body string
	// Head and Base are branch names; HeadSHA empty reads the head branch's commit.
	Head, Base string
	HeadSHA    string
	// State is open or closed; empty is open.
	State          string
	Merged         bool
	MergedAt       *time.Time
	MergeCommitSHA string
	// Mergeable is GitHub's nullable mergeable; MergeableState its mergeable_state.
	Mergeable      *bool
	MergeableState string
	UpdatedAt      time.Time
}

// Review is a review of a pull request.
type Review struct {
	ID          int64
	User        string
	State       string
	Body        string
	CommitID    string
	SubmittedAt time.Time
}

// Activity is an entry of a branch's activity (GET /activity): what moved it, from which commit to
// which, when, and who.
type Activity struct {
	Type          string
	Before, After string
	Actor         string
	ActorType     string
	Timestamp     time.Time
}

// AddPullRequest adds pr to the repository slug names and returns it as the server holds it: the
// next number when it names none, open when it names no state, updated now when it names no time.
func (s *Server) AddPullRequest(t testing.TB, slug string, pr PullRequest) PullRequest {
	t.Helper()
	s.mu.Lock()
	repo := s.served(t, slug)
	if pr.Number == 0 {
		pr.Number = len(repo.pulls) + 1
	}
	pr.State = cmp.Or(pr.State, "open")
	if pr.UpdatedAt.IsZero() {
		pr.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	}
	held := pr
	repo.pulls = append(repo.pulls, &held)
	s.mu.Unlock()
	// The webhook's pull_request event of the opening (webhook.go), naming the head the branch
	// holds when HeadSHA names none.
	head := pr.HeadSHA
	if head == "" {
		head, _ = s.gitOutput(repo.Bare, "", "rev-parse", "refs/heads/"+pr.Head)
	}
	s.opened(repo, pr, strings.TrimSpace(head))
	return pr
}

// AddReview adds review to pull request number of slug and returns it with its id.
func (s *Server) AddReview(t testing.TB, slug string, number int, review Review) Review {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.served(t, slug)
	if review.ID == 0 {
		review.ID = s.nextID()
	}
	repo.reviews[number] = append(repo.reviews[number], review)
	return review
}

// SetPermission gives login permission (admin, maintain, write, triage or read) on slug; a login
// given none is no collaborator, which GitHub answers 404.
func (s *Server) SetPermission(t testing.TB, slug, login, permission string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served(t, slug).permissions[strings.ToLower(login)] = permission
}

// AddActivity adds entry, the newest, to branch's activity on slug.
func (s *Server) AddActivity(t testing.TB, slug, branch string, entry Activity) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := s.served(t, slug)
	ref := "refs/heads/" + branch
	repo.activity[ref] = append([]Activity{entry}, repo.activity[ref]...)
}

// served is the repository slug names, failing the test when the server serves none; s.mu is held.
func (s *Server) served(t testing.TB, slug string) *repository {
	t.Helper()
	owner, name, _ := strings.Cut(slug, "/")
	repo := s.repos[key(owner, name)]
	if repo == nil {
		t.Fatalf("the fake GitHub serves no repository %s", slug)
	}
	return repo
}

// caller is who a request's Authorization names: an App by its verified JWT, a token the server
// takes, nobody, or a credential it refuses.
type caller struct {
	app     *app
	grant   *grant
	refused bool
}

// callerOf reads r's Authorization, `Bearer` or `token`, as GitHub does; s.mu is held.
func (s *Server) callerOf(r *http.Request) caller {
	header := r.Header.Get("Authorization")
	if header == "" {
		return caller{}
	}
	scheme, credential, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "bearer") && !strings.EqualFold(scheme, "token") {
		return caller{refused: true}
	}
	if found := s.appOf(credential); found != nil {
		return caller{app: found}
	}
	if held := s.tokens[credential]; held != nil && held.live() {
		return caller{grant: held}
	}
	return caller{refused: true}
}

// appOf is the App whose key signed jwt, an RS256 App JWT whose iss is the App's id and which has
// not expired; nil for anything else. s.mu is held.
func (s *Server) appOf(jwt string) *app {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Issuer  json.RawMessage `json:"iss"`
		Expires int64           `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || time.Now().Unix() >= claims.Expires {
		return nil
	}
	id, err := strconv.ParseInt(strings.Trim(string(claims.Issuer), `"`), 10, 64)
	if err != nil {
		return nil
	}
	found := s.apps[id]
	if found == nil {
		return nil
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(found.key, crypto.SHA256, digest[:], signature) != nil {
		return nil
	}
	return found
}

// answer writes v as GitHub's JSON with status.
func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// refuse writes GitHub's error body, {"message": message}, with status.
func refuse(w http.ResponseWriter, status int, message string) {
	answer(w, status, map[string]string{"message": message, "documentation_url": "https://docs.github.com/rest"})
}

func notFound(w http.ResponseWriter) { refuse(w, http.StatusNotFound, "Not Found") }

// serveAPI answers one request of the REST or GraphQL API.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	who := s.callerOf(r)
	s.mu.Unlock()
	if who.refused {
		refuse(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	get := r.Method == http.MethodGet
	switch {
	case r.URL.Path == "/graphql" && r.Method == http.MethodPost:
		s.graphQL(w, r, who)
	case segments[0] == "app":
		s.appAPI(w, r, who, segments[1:])
	case segments[0] == "apps" && len(segments) == 2 && get:
		s.appBySlug(w, segments[1])
	case segments[0] == "users" && len(segments) == 2 && get:
		s.user(w, segments[1])
	case r.URL.Path == "/installation/repositories" && get:
		s.installationRepositories(w, r, who)
	case segments[0] == "repos" && len(segments) >= 3:
		s.repositoryAPI(w, r, who, segments[1], segments[2], segments[3:])
	default:
		notFound(w)
	}
}

// appAPI answers the App's own endpoints, each under its JWT alone.
func (s *Server) appAPI(w http.ResponseWriter, r *http.Request, who caller, rest []string) {
	if who.app == nil {
		refuse(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	found := who.app
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		answer(w, http.StatusOK, map[string]any{"id": found.ID, "slug": found.Slug, "name": found.Slug})
	case len(rest) == 1 && rest[0] == "installations" && r.Method == http.MethodGet:
		// One page holds every installation; a later page holds none, which ends appauth's
		// discovery as GitHub's last page does.
		listed := []map[string]any{}
		if atoi(r.URL.Query().Get("page")) <= 1 {
			for _, id := range sortedIDs(found.owners) {
				listed = append(listed, s.installation(found, id))
			}
		}
		answer(w, http.StatusOK, listed)
	case len(rest) >= 2 && rest[0] == "installations":
		id, _ := strconv.ParseInt(rest[1], 10, 64)
		if _, ok := found.owners[id]; !ok {
			notFound(w)
			return
		}
		switch {
		case len(rest) == 2 && r.Method == http.MethodGet:
			answer(w, http.StatusOK, s.installation(found, id))
		case len(rest) == 3 && rest[2] == "access_tokens" && r.Method == http.MethodPost:
			s.mint(w, r, found, id)
		default:
			notFound(w)
		}
	default:
		notFound(w)
	}
}

func sortedIDs(owners map[int64]string) []int64 {
	ids := make([]int64, 0, len(owners))
	for id := range owners {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// installation is installation id of found as GitHub answers it.
func (s *Server) installation(found *app, id int64) map[string]any {
	return map[string]any{
		"id": id, "app_id": found.ID, "app_slug": found.Slug,
		"account":              map[string]any{"login": found.owners[id], "type": "Organization"},
		"permissions":          grantedPermissions,
		"repository_selection": "all",
		"suspended_at":         nil,
	}
}

// mint answers an installation token request: a token holding exactly the permissions the request
// names (every granted one when it names none), which may write when it holds contents: write, for
// an hour. A permission asked at more than the installation is granted is refused 422, as GitHub
// refuses it.
func (s *Server) mint(w http.ResponseWriter, r *http.Request, found *app, installation int64) {
	var request struct {
		Permissions map[string]string `json:"permissions"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			refuse(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
	}
	permissions := request.Permissions
	if len(permissions) == 0 {
		permissions = grantedPermissions
	}
	for permission, level := range permissions {
		if access[level] == 0 || access[level] > access[grantedPermissions[permission]] {
			refuse(w, http.StatusUnprocessableEntity, "The permissions requested are not granted to this installation.")
			return
		}
	}
	write := permissions["contents"] == "write"
	value, expires := s.grantInstallation(found, installation, write)
	answer(w, http.StatusCreated, map[string]any{
		"token": value, "expires_at": expires.Format(time.RFC3339), "permissions": permissions, "repository_selection": "all",
	})
}

// InstallationToken mints a token of app's installation on owner, as POST
// /app/installations/{id}/access_tokens does under the App's own JWT: one that may push when write
// is true and only reads otherwise, reaching owner's repositories alone, for an hour. It is what a
// test's stand-in for a secrets broker's release of an App's tokens hands out (appauth's token
// mode, internal/kindtest's AppTokenCommand). An owner the App is not installed on fails t.
func (s *Server) InstallationToken(t testing.TB, app App, owner string, write bool) (token string, installation int64, expires time.Time) {
	t.Helper()
	s.mu.Lock()
	found := s.apps[app.ID]
	if found != nil {
		installation = found.installations[strings.ToLower(owner)]
	}
	s.mu.Unlock()
	if installation == 0 {
		t.Errorf("the fake GitHub's App %s is not installed on %s", app.Slug, owner)
		return "", 0, time.Time{}
	}
	token, expires = s.grantInstallation(found, installation, write)
	return token, installation, expires
}

// grantInstallation has the server take a fresh token of found's installation, for an hour, or for
// what SetTokenLifetime set: its bot's, reaching that installation's owner alone, and one that may
// push when write is true.
func (s *Server) grantInstallation(found *app, installation int64, write bool) (string, time.Time) {
	secret := make([]byte, 18)
	_, _ = rand.Read(secret)
	value := "ghs_" + hex.EncodeToString(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	expires := time.Now().Add(cmp.Or(s.tokenLifetime, time.Hour)).UTC().Truncate(time.Second)
	s.tokens[value] = &grant{write: write, login: found.Slug + "[bot]",
		owner: found.owners[installation], installation: installation, expires: expires}
	return value, expires
}

// SetTokenLifetime has every installation token minted from now on live for lifetime rather than
// GitHub's hour, so a test sees a holder of one refresh it within the test's own time.
func (s *Server) SetTokenLifetime(lifetime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenLifetime = lifetime
}

// appBySlug is GET /apps/{slug}: the App's id, which token mode holds to its configured app_id.
func (s *Server) appBySlug(w http.ResponseWriter, slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, found := range s.apps {
		if strings.EqualFold(found.Slug, slug) {
			answer(w, http.StatusOK, map[string]any{"id": found.ID, "slug": found.Slug, "name": found.Slug})
			return
		}
	}
	notFound(w)
}

// user is GET /users/{login}: an App's bot by the id GitHub gave it, and any other login as a user
// whose id is the same on every read.
func (s *Server) user(w http.ResponseWriter, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, found := range s.apps {
		if strings.EqualFold(found.Slug+"[bot]", login) {
			answer(w, http.StatusOK, map[string]any{"id": found.botID, "login": found.Slug + "[bot]", "type": "Bot"})
			return
		}
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.ToLower(login)))
	answer(w, http.StatusOK, map[string]any{"id": int64(hash.Sum32()) + 1, "login": login, "type": "User"})
}

// installationRepositories is GET /installation/repositories under an installation's token: every
// served repository of its owner, on one page.
func (s *Server) installationRepositories(w http.ResponseWriter, r *http.Request, who caller) {
	if who.grant == nil || who.grant.installation == 0 {
		refuse(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	s.mu.Lock()
	listed := []map[string]any{}
	if atoi(r.URL.Query().Get("page")) <= 1 {
		for _, k := range sortedKeys(s.repos) {
			if repo := s.repos[k]; who.grant.reaches(repo) {
				listed = append(listed, s.repositoryJSON(repo))
			}
		}
	}
	s.mu.Unlock()
	answer(w, http.StatusOK, map[string]any{"total_count": len(listed), "repository_selection": "all", "repositories": listed})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// visible says whether who may read repo: anyone a public one, and a token that reaches it a
// private one.
func visible(repo *repository, who caller) bool {
	return repo.Public || who.grant != nil && who.grant.reaches(repo)
}

// repositoryAPI answers /repos/{owner}/{name}/...
func (s *Server) repositoryAPI(w http.ResponseWriter, r *http.Request, who caller, owner, name string, rest []string) {
	s.mu.Lock()
	repo := s.repos[key(owner, name)]
	s.mu.Unlock()
	get := r.Method == http.MethodGet
	if len(rest) == 1 && rest[0] == "installation" && get {
		s.repositoryInstallation(w, who, owner)
		return
	}
	if repo == nil || !visible(repo, who) {
		notFound(w)
		return
	}
	switch {
	case len(rest) == 0 && get:
		s.mu.Lock()
		body := s.repositoryJSON(repo)
		s.mu.Unlock()
		answer(w, http.StatusOK, body)
	case len(rest) >= 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads" && get:
		s.readRef(w, repo, "refs/heads/"+strings.Join(rest[3:], "/"))
	case len(rest) == 2 && rest[0] == "git" && rest[1] == "refs" && r.Method == http.MethodPost:
		s.createRef(w, r, repo, who)
	case len(rest) == 1 && rest[0] == "pulls" && get:
		s.listPulls(w, r, repo)
	case len(rest) == 2 && rest[0] == "pulls" && get:
		s.withPull(w, repo, rest[1], func(pr *PullRequest) { answer(w, http.StatusOK, s.pullJSON(repo, pr, false)) })
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "reviews" && get:
		s.withPull(w, repo, rest[1], func(pr *PullRequest) {
			listed := []map[string]any{}
			if atoi(r.URL.Query().Get("page")) <= 1 {
				s.mu.Lock()
				for _, review := range repo.reviews[pr.Number] {
					listed = append(listed, reviewJSON(review))
				}
				s.mu.Unlock()
			}
			answer(w, http.StatusOK, listed)
		})
	case len(rest) == 4 && rest[0] == "pulls" && rest[2] == "reviews" && get:
		s.withPull(w, repo, rest[1], func(pr *PullRequest) {
			id, _ := strconv.ParseInt(rest[3], 10, 64)
			s.mu.Lock()
			reviews := slices.Clone(repo.reviews[pr.Number])
			s.mu.Unlock()
			for _, review := range reviews {
				if review.ID == id {
					answer(w, http.StatusOK, reviewJSON(review))
					return
				}
			}
			notFound(w)
		})
	case len(rest) == 3 && rest[0] == "collaborators" && rest[2] == "permission" && get:
		s.mu.Lock()
		permission, ok := repo.permissions[strings.ToLower(rest[1])]
		s.mu.Unlock()
		if !ok {
			refuse(w, http.StatusNotFound, rest[1]+" is not a user")
			return
		}
		answer(w, http.StatusOK, map[string]any{"permission": permission, "role_name": permission,
			"user": map[string]any{"login": rest[1], "type": "User"}})
	case len(rest) == 1 && rest[0] == "activity" && get:
		listed := []map[string]any{}
		if atoi(r.URL.Query().Get("page")) <= 1 {
			ref := r.URL.Query().Get("ref")
			s.mu.Lock()
			for _, entry := range repo.activity[ref] {
				listed = append(listed, map[string]any{"activity_type": entry.Type, "before": entry.Before, "after": entry.After,
					"ref": ref, "timestamp": entry.Timestamp.UTC().Format(time.RFC3339),
					"actor": map[string]any{"login": entry.Actor, "type": cmp.Or(entry.ActorType, "User")}})
			}
			s.mu.Unlock()
		}
		answer(w, http.StatusOK, listed)
	case len(rest) == 2 && rest[0] == "tarball" && get:
		s.tarball(w, repo, rest[1])
	default:
		notFound(w)
	}
}

// repositoryInstallation is GET /repos/{owner}/{repo}/installation under an App's JWT: its
// installation on owner, which a repository of that owner is installed on.
func (s *Server) repositoryInstallation(w http.ResponseWriter, who caller, owner string) {
	if who.app == nil {
		refuse(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := who.app.installations[strings.ToLower(owner)]
	if !ok {
		notFound(w)
		return
	}
	answer(w, http.StatusOK, s.installation(who.app, id))
}

// repositoryJSON is repo as GET /repos/{owner}/{repo} answers it; s.mu is held.
func (s *Server) repositoryJSON(repo *repository) map[string]any {
	visibility := "private"
	if repo.Public {
		visibility = "public"
	}
	branch, err := s.gitOutput(repo.Bare, "", "symbolic-ref", "--short", "HEAD")
	if err != nil {
		branch = "main"
	}
	return map[string]any{
		"id": repo.id, "name": repo.Name, "full_name": repo.Slug(), "private": !repo.Public, "visibility": visibility,
		"default_branch": branch, "owner": map[string]any{"login": repo.Owner, "type": "Organization"},
		"html_url": repo.URL(), "clone_url": repo.URL() + ".git",
	}
}

// readRef is GET /git/ref/heads/{branch}: the commit the branch names in the bare repository.
func (s *Server) readRef(w http.ResponseWriter, repo *repository, ref string) {
	sha, err := s.gitOutput(repo.Bare, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || sha == "" {
		notFound(w)
		return
	}
	answer(w, http.StatusOK, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}})
}

// createRef is POST /git/refs: a ref at a commit the repository holds, under a token that may
// write, refused 422 where the ref is already there or the commit is not, as GitHub refuses each.
func (s *Server) createRef(w http.ResponseWriter, r *http.Request, repo *repository, who caller) {
	if who.grant == nil || !who.grant.write {
		refuse(w, http.StatusForbidden, "Resource not accessible by integration")
		return
	}
	var request struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !strings.HasPrefix(request.Ref, "refs/") || request.SHA == "" {
		refuse(w, http.StatusUnprocessableEntity, "Reference name and sha are required")
		return
	}
	if _, err := s.gitOutput(repo.Bare, "", "cat-file", "-e", request.SHA+"^{commit}"); err != nil {
		refuse(w, http.StatusUnprocessableEntity, "Object does not exist")
		return
	}
	if _, err := s.gitOutput(repo.Bare, "", "check-ref-format", request.Ref); err != nil {
		refuse(w, http.StatusUnprocessableEntity, "Reference name is not valid")
		return
	}
	if _, err := s.gitOutput(repo.Bare, "", "update-ref", request.Ref, request.SHA, ""); err != nil {
		refuse(w, http.StatusUnprocessableEntity, "Reference already exists")
		return
	}
	answer(w, http.StatusCreated, map[string]any{"ref": request.Ref, "object": map[string]any{"sha": request.SHA, "type": "commit"}})
}

// listPulls is GET /pulls: the repository's pull requests in the state the query names (open when
// it names none, or all), whose head is the query's owner:branch and whose base is its base when
// it names them, newest first, in the list's own shape.
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, repo *repository) {
	query := r.URL.Query()
	state := cmp.Or(query.Get("state"), "open")
	headOwner, headBranch, named := strings.Cut(query.Get("head"), ":")
	s.mu.Lock()
	pulls := slices.Clone(repo.pulls)
	s.mu.Unlock()
	listed := []map[string]any{}
	if atoi(query.Get("page")) > 1 {
		answer(w, http.StatusOK, listed)
		return
	}
	for _, pr := range slices.Backward(pulls) {
		switch {
		case state != "all" && pr.State != state:
		case named && (!strings.EqualFold(headOwner, repo.Owner) || pr.Head != headBranch):
		case query.Get("base") != "" && pr.Base != query.Get("base"):
		default:
			listed = append(listed, s.pullJSON(repo, pr, true))
		}
	}
	answer(w, http.StatusOK, listed)
}

// withPull calls found with pull request number of repo, or answers 404.
func (s *Server) withPull(w http.ResponseWriter, repo *repository, number string, found func(*PullRequest)) {
	n := atoi(number)
	s.mu.Lock()
	var pull *PullRequest
	for _, pr := range repo.pulls {
		if pr.Number == n {
			held := *pr
			pull = &held
		}
	}
	s.mu.Unlock()
	if pull == nil {
		notFound(w)
		return
	}
	found(pull)
}

// pullJSON is pr as GitHub answers it: listed leaves out merged, mergeable and mergeable_state,
// which a list does not carry (merged_at says it there).
func (s *Server) pullJSON(repo *repository, pr *PullRequest, listed bool) map[string]any {
	head := pr.HeadSHA
	if head == "" {
		head, _ = s.gitOutput(repo.Bare, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+pr.Head+"^{commit}")
	}
	var mergedAt any
	if pr.MergedAt != nil {
		mergedAt = pr.MergedAt.UTC().Format(time.RFC3339)
	}
	body := map[string]any{
		"number": pr.Number, "state": pr.State, "title": pr.Title, "body": pr.Body,
		"html_url": fmt.Sprintf("%s/pull/%d", repo.URL(), pr.Number),
		"base":     map[string]any{"ref": pr.Base, "repo": map[string]any{"full_name": repo.Slug()}},
		"head": map[string]any{"ref": pr.Head, "sha": head, "label": repo.Owner + ":" + pr.Head,
			"repo": map[string]any{"full_name": repo.Slug()}},
		"updated_at": pr.UpdatedAt.UTC().Format(time.RFC3339), "merged_at": mergedAt,
		"merge_commit_sha": pr.MergeCommitSHA,
	}
	if !listed {
		body["merged"] = pr.Merged
		body["mergeable"] = pr.Mergeable
		body["mergeable_state"] = cmp.Or(pr.MergeableState, "unknown")
	}
	return body
}

func reviewJSON(review Review) map[string]any {
	return map[string]any{"id": review.ID, "user": map[string]any{"login": review.User}, "state": review.State,
		"body": review.Body, "commit_id": review.CommitID, "submitted_at": review.SubmittedAt.UTC().Format(time.RFC3339)}
}

// tarball is GET /tarball/{ref}: git archive's gzipped tar of the commit ref names, under the
// <owner>-<name>-<short sha>/ prefix GitHub's archives carry. GitHub answers with a redirect to
// its download host; the server answers the archive itself, which a client following redirects
// reads the same.
func (s *Server) tarball(w http.ResponseWriter, repo *repository, ref string) {
	sha, err := s.gitOutput(repo.Bare, "", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || sha == "" {
		notFound(w)
		return
	}
	prefix := fmt.Sprintf("%s-%s-%s/", repo.Owner, repo.Name, sha[:7])
	out, err := s.gitCommand(repo.Bare, "archive", "--format=tar.gz", "--prefix="+prefix, sha).Output()
	if err != nil {
		refuse(w, http.StatusInternalServerError, "git archive: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strings.TrimSuffix(prefix, "/")+".tar.gz")
	_, _ = w.Write(out)
}

// graphQL answers the two GraphQL documents the daemon and the leak guard send: `{viewer{login}}`
// (token mode's check of a token's bot) and the leak guard's repository visibility read
// (leakguard.VisibilityGraphQLArgs), with its owner and name as variables. GitHub's GraphQL API
// answers no request without a credential.
func (s *Server) graphQL(w http.ResponseWriter, r *http.Request, who caller) {
	if who.grant == nil {
		refuse(w, http.StatusUnauthorized, "This endpoint requires you to be authenticated.")
		return
	}
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		refuse(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	switch {
	case strings.Contains(request.Query, "viewer"):
		answer(w, http.StatusOK, map[string]any{"data": map[string]any{"viewer": map[string]any{"login": who.grant.login}}})
	case strings.Contains(request.Query, "repository("):
		owner, _ := request.Variables["owner"].(string)
		name, _ := request.Variables["name"].(string)
		s.mu.Lock()
		repo := s.repos[key(owner, name)]
		s.mu.Unlock()
		if repo == nil || !visible(repo, who) {
			answer(w, http.StatusOK, map[string]any{
				"data": map[string]any{"repository": nil},
				"errors": []map[string]any{{"type": "NOT_FOUND", "path": []string{"repository"},
					"message": fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, name)}},
			})
			return
		}
		visibility := "PRIVATE"
		if repo.Public {
			visibility = "PUBLIC"
		}
		answer(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
			"nameWithOwner": repo.Slug(), "visibility": visibility}}})
	default:
		answer(w, http.StatusOK, map[string]any{"errors": []map[string]any{{
			"message": "the fake GitHub answers the viewer and repository visibility queries alone"}}})
	}
}
