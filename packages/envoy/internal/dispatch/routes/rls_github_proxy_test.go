package routes

// rls_github_proxy_test.go holds the GitHub proxy rows of plan.md (INFRA-6168, "The GitHub proxy
// rows", G1-G7): the proxy reads a repository for a person only when an issue the person sees
// resolves to it, or no issue of any project does and the person is an open viewer; it answers
// every other repository as one the App is not installed on and asks GitHub nothing of it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The proxy rows' people, each signed in by the header identity.
const (
	// proxyOwner is a full member who owns every project.
	proxyOwner = "olga@rls.example"
	// proxyOpen is a full member no list names: an open viewer, who sees every public project and
	// no private one.
	proxyOpen = "alice@rls.example"
	// proxyListed is a full member PRIV's list names, and PRIV2's does not.
	proxyListed = "carol@rls.example"
	// proxyLimited is a limited member PUB's list names: she sees PUB alone.
	proxyLimited = "lena@rls.example"
)

// The repositories of the proxy rows, each one way an issue resolves to a repository or none does.
const (
	// repoPub is PUB-1's own.
	repoPub = "acme/pub"
	// repoBoth is PUB-2's own and PRIV-4's: a visible and a hidden project share it.
	repoBoth = "acme/both"
	// repoPriv is PRIV-1's own.
	repoPriv = "acme/priv"
	// repoPrivParent is PRIV2-1's own, and PRIV-2's through its parent PRIV2-1: proxyListed, who
	// sees PRIV and not PRIV2, reaches it through the parent alone.
	repoPrivParent = "acme/privparent"
	// repoPrivDefault is PRIV's default, which PRIV-3, naming none, resolves to.
	repoPrivDefault = "acme/privdefault"
	// repoNone is a repository no issue resolves to.
	repoNone = "acme/none"
	// repoOld is a repository no issue resolves to, whose pull request GitHub answers with a
	// redirect to repoNew's, as it answers a renamed or transferred repository's old name.
	repoOld = "acme/old"
	repoNew = "acme/new"
	// repoUninstalled is a repository the App is not installed on: every hidden repository's
	// answer must equal its answer.
	repoUninstalled = "acme/uninstalled"
)

// proxyInstalled is every repository the App is installed on.
var proxyInstalled = []string{repoPub, repoBoth, repoPriv, repoPrivParent, repoPrivDefault, repoNone, repoOld, repoNew}

// proxyGitHub is GitHub for the App installed on proxyInstalled: its installation on each, a token
// for it, a redirect of repoOld's pull request to repoNew's, and every other read answered with the
// repository's name. Every request it is sent is recorded (githubtest.Spec.Record), the
// installation lookups and token mints included, and every read of a repository apart.
type proxyGitHub struct {
	mu       sync.Mutex
	requests []string
	reads    []string
}

func (g *proxyGitHub) record(r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.Method+" "+r.URL.RequestURI())
}

func (g *proxyGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/11/access_tokens" {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_rls","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	owner, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	name, path, _ := strings.Cut(rest, "/")
	repository := owner + "/" + name
	if path == "installation" {
		if !slices.Contains(proxyInstalled, repository) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read","checks":"read"}}`)
		return
	}
	g.mu.Lock()
	g.reads = append(g.reads, r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	if repository == repoOld {
		http.Redirect(w, r, "/repos/"+repoNew+"/"+path, http.StatusMovedPermanently)
		return
	}
	fmt.Fprintf(w, `{"repository":%q}`, repository)
}

// asked is how many requests GitHub has been sent, and read how many reads of a repository.
func (g *proxyGitHub) asked() (requests, reads int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests), len(g.reads)
}

// readsSince is every read of a repository after the first n.
func (g *proxyGitHub) readsSince(n int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reads[n:])
}

// seedProxyProjects writes the proxy rows' people, projects and issues. With private, PRIV and
// PRIV2 are private, PRIV's list names proxyListed, PUB's names proxyLimited, and proxyLimited is
// a limited member; without it every project is public and every person a full member.
func seedProxyProjects(t *testing.T, private bool) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	visibility, kind := "public", "full"
	if private {
		visibility, kind = "private", "restricted"
	}
	if _, err := database.Pool.Exec(context.Background(), fmt.Sprintf(`
		insert into people (email, member_kind) values
			('%[1]s', 'full'), ('%[2]s', 'full'), ('%[3]s', 'full'), ('%[4]s', '%[5]s');
		insert into projects (key, name, default_assignee, visibility, default_repo) values
			('PUB', 'Public', '%[1]s', 'public', null),
			('PRIV', 'Private', '%[1]s', '%[6]s', '%[7]s'),
			('PRIV2', 'Private two', '%[1]s', '%[6]s', null);
		update projects set owner = '%[1]s';
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', '%[3]s', '{"kind":"user","id":"%[1]s"}'),
			('PUB', 'person', '%[4]s', '{"kind":"user","id":"%[1]s"}');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-1', 'PUB', 1, 'Names %[8]s', '{"kind":"user","id":"%[1]s"}', 'a', null, '%[8]s', 'triage'),
			('PUB-2', 'PUB', 2, 'Names %[9]s', '{"kind":"user","id":"%[1]s"}', 'b', null, '%[9]s', 'triage'),
			('PRIV2-1', 'PRIV2', 1, 'Names %[10]s', '{"kind":"user","id":"%[1]s"}', 'a', null, '%[10]s', 'triage'),
			('PRIV-1', 'PRIV', 1, 'Names %[11]s', '{"kind":"user","id":"%[1]s"}', 'a', null, '%[11]s', 'triage'),
			('PRIV-2', 'PRIV', 2, 'Under PRIV2-1', '{"kind":"user","id":"%[1]s"}', 'b', 'PRIV2-1', null, 'triage'),
			('PRIV-3', 'PRIV', 3, 'Names none', '{"kind":"user","id":"%[1]s"}', 'c', null, null, 'triage'),
			('PRIV-4', 'PRIV', 4, 'Names %[9]s', '{"kind":"user","id":"%[1]s"}', 'd', null, '%[9]s', 'done');
	`, proxyOwner, proxyOpen, proxyListed, proxyLimited, kind, visibility, repoPrivDefault, repoPub, repoBoth, repoPrivParent, repoPriv)); err != nil {
		t.Fatalf("seed the proxy rows' projects: %v", err)
	}
	return database
}

// proxyRouter is the router over database as production builds it, its Store the request store
// and System database, its GitHub fake, with the App's key, its callers named by the
// X-Dispatch-User header as a person at the browser.
func proxyRouter(t *testing.T, database *store.Store, fake *proxyGitHub) http.Handler {
	t.Helper()
	_, pem := githubapptest.Key(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	signIn := identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
	request := storetest.Request(t, database)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Request: request, Events: broker, Identity: signIn})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    testSigningKey,
		People:        people,
		Identity:      signIn,
		Store:         request,
		System:        database,
		Docs:          documents,
		Events:        broker,
		App:           &auth.AppConfig{ClientID: "Iv1.rls", PEM: pem},
		GitHubAPIBase: githubtest.Server(t, githubtest.Spec{Record: fake.record, Handle: fake.serve}),
		Lifetime:      t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return handler
}

// proxyRead is login's read of repository's first pull request through the proxy, as
// "<status> <Retry-After> <body>", with the repository's own name written as <repo>, the one byte
// of an answer two repositories may differ in.
func proxyRead(t *testing.T, handler http.Handler, login, repository string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/github/rest/repos/"+repository+"/pulls/1", nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return strings.ReplaceAll(fmt.Sprintf("%d %q %s", response.Code, response.Header().Get("Retry-After"),
		strings.TrimSpace(response.Body.String())), repository, "<repo>")
}

// proxied is the answer of a read GitHub answered with the repository's name.
const proxied = `200 "" {"repository":"<repo>"}`

// proxyRows is one router over one seed, and the answer a repository the App is not installed on
// gets, which every hidden repository's answer must equal.
type proxyRows struct {
	handler     http.Handler
	fake        *proxyGitHub
	uninstalled string
}

func newProxyRows(t *testing.T, private bool) proxyRows {
	t.Helper()
	fake := &proxyGitHub{}
	rows := proxyRows{handler: proxyRouter(t, seedProxyProjects(t, private), fake), fake: fake}
	rows.uninstalled = proxyRead(t, rows.handler, proxyOpen, repoUninstalled)
	if !strings.HasPrefix(rows.uninstalled, `503 "" `) || !strings.Contains(rows.uninstalled, "is not installed on <repo>") {
		t.Fatalf("a read of %s, which the App is not installed on, answered %s: the comparison proves nothing", repoUninstalled, rows.uninstalled)
	}
	return rows
}

// requireProxied fails t unless login's read of each of repositories is GitHub's answer, read
// from GitHub.
func (rows proxyRows) requireProxied(t *testing.T, login string, repositories ...string) {
	t.Helper()
	for _, repository := range repositories {
		_, before := rows.fake.asked()
		got := proxyRead(t, rows.handler, login, repository)
		if _, after := rows.fake.asked(); got != proxied || after == before {
			t.Errorf("%s reads %s as %s (GitHub read: %t), want GitHub's answer %s", login, repository, got, after > before, proxied)
		}
	}
}

// requireHidden fails t unless login's read of each of repositories answers what a repository the
// App is not installed on answers, with GitHub sent no request at all.
func (rows proxyRows) requireHidden(t *testing.T, login string, repositories ...string) {
	t.Helper()
	for _, repository := range repositories {
		before, _ := rows.fake.asked()
		got := proxyRead(t, rows.handler, login, repository)
		if after, _ := rows.fake.asked(); got != rows.uninstalled || after != before {
			t.Errorf("%s reads %s as\n\t%s\nwith GitHub sent %d requests; want what %s, which the App is not installed on, answers, with GitHub sent nothing:\n\t%s",
				login, repository, got, after-before, repoUninstalled, rows.uninstalled)
		}
	}
}

// TestGitHubProxyEnforcesViewerRepositories (plan R4, D11 "GitHub proxy"): a person reads through
// the proxy a repository an issue they see resolves to, through its own repository, its parent's
// or its project's default, and, as an open viewer, a repository no issue resolves to; every other
// repository answers as one the App is not installed on, GitHub asked nothing, and while a
// project is private no redirect to another repository is followed. With no private project and no
// limited member every repository is proxied as on main (R5, G7).
func TestGitHubProxyEnforcesViewerRepositories(t *testing.T) {
	t.Parallel()
	t.Run("G1_a_repository_a_visible_issue_resolves_to_is_proxied", func(t *testing.T) {
		rows := newProxyRows(t, true)
		rows.requireProxied(t, proxyListed, repoPriv, repoPrivParent, repoPrivDefault)
		rows.requireProxied(t, proxyOpen, repoPub)
	})
	t.Run("G2_a_repository_only_a_hidden_project_resolves_to_answers_not_installed", func(t *testing.T) {
		rows := newProxyRows(t, true)
		rows.requireHidden(t, proxyOpen, repoPriv, repoPrivParent, repoPrivDefault)
	})
	t.Run("G3_a_repository_a_visible_and_a_hidden_project_share_is_proxied", func(t *testing.T) {
		rows := newProxyRows(t, true)
		rows.requireProxied(t, proxyOpen, repoBoth)
	})
	t.Run("G4_a_repository_no_issue_resolves_to_is_proxied_to_an_open_viewer", func(t *testing.T) {
		rows := newProxyRows(t, true)
		rows.requireProxied(t, proxyOpen, repoNone)
	})
	t.Run("G5_a_repository_no_issue_resolves_to_answers_not_installed_to_a_limited_member", func(t *testing.T) {
		rows := newProxyRows(t, true)
		rows.requireProxied(t, proxyLimited, repoPub)
		rows.requireHidden(t, proxyLimited, repoNone)
	})
	t.Run("G6_a_redirect_to_another_repository_while_a_project_is_private_answers_not_installed", func(t *testing.T) {
		rows := newProxyRows(t, true)
		_, before := rows.fake.asked()
		got := proxyRead(t, rows.handler, proxyOpen, repoOld)
		if read := rows.fake.readsSince(before); got != rows.uninstalled || slices.Contains(read, "GET /repos/"+repoNew+"/pulls/1") {
			t.Errorf("while PRIV is private, %s reads %s, which GitHub redirects to %s, as\n\t%s\nwith GitHub read %q; want what %s answers, and %s not read:\n\t%s",
				proxyOpen, repoOld, repoNew, got, read, repoUninstalled, repoNew, rows.uninstalled)
		}
	})
	t.Run("G7_all_public_every_repository_proxied_as_on_main", func(t *testing.T) {
		rows := newProxyRows(t, false)
		for _, login := range []string{proxyOpen, proxyLimited} {
			rows.requireProxied(t, login, repoPub, repoBoth, repoPriv, repoPrivParent, repoPrivDefault, repoNone)
			_, before := rows.fake.asked()
			want := fmt.Sprintf(`200 "" {"repository":%q}`, repoNew)
			got := proxyRead(t, rows.handler, login, repoOld)
			if read := rows.fake.readsSince(before); got != want || !slices.Contains(read, "GET /repos/"+repoNew+"/pulls/1") {
				t.Errorf("with every project public, %s reads %s, which GitHub redirects to %s, as %s with GitHub read %q; want GitHub's %s, read from %s",
					login, repoOld, repoNew, got, read, want, repoNew)
			}
		}
	})
}
