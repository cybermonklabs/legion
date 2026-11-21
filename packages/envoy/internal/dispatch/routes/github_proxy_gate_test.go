package routes

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
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// gateRead is the GitHub proxy's read the no-database test makes: one pull request of acme/web.
const gateRead = "/api/github/rest/repos/acme/web/pulls/1"

// A router with no database has no viewer to resolve for the cookie's person, so its proxy gate
// fails closed: a repository the App is installed on answers as one it is not, and GitHub is not
// read.
func TestTheGitHubProxyOfARouterWithNoDatabaseHidesEveryRepository(t *testing.T) {
	fake := &gateGitHub{installed: []string{"acme/web"}}
	_, pem := githubapptest.Key(t)
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    testSigningKey,
		People:        people,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		App:           &auth.AppConfig{ClientID: "Iv1.gate", PEM: pem},
		GitHubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: fake.serve}),
		Lifetime:      t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, _ := New(ctx)
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+gateRead, nil)
	request.Header.Set("X-Dispatch-User", "alice@example.com")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "not installed on acme/web") {
		t.Fatalf("a router with no database answered %s as %d %s, want 503 not installed", gateRead, response.Code, response.Body.String())
	}
	if reads := fake.repositoryReads(); len(reads) != 0 {
		t.Fatalf("GitHub was read through a router with no database: %q", reads)
	}
}

// proxiedRepositories are the nine repositories of the proxy gate's seed (seedProxyGate), each one
// way an issue resolves to a repository or none does. The App is installed on every one of them.
var proxiedRepositories = []string{
	"acme/pub", "acme/priv", "acme/privdone", "acme/privparent", "acme/privdefault", "acme/none", "acme/both",
	"acme/oldname", "acme/newname",
}

// uninstalledRepository is a repository the App is not installed on, whose answer every hidden
// repository's answer must equal.
const uninstalledRepository = "acme/uninstalled"

// TestTheGitHubProxyShowsARepositoryByItsIssues (c-proxy-by-issue): the proxy shows a person a
// repository that an issue of a project the person sees resolves to, and an open viewer also one
// that no project the person does not see owns; it answers every other repository, in status and
// bytes, as one the App is not installed on, reading nothing of it from GitHub. An issue resolves
// through its own repository, its parent's and its project's default, a done issue counts as an
// open one does, and no rename a mapping records is read.
//
// alice, a full member no private list names, reads acme/pub, acme/none, acme/both, which PUB-2
// names beside PRIV-5, and acme/newname, which no issue names; bob, a restricted member PRIV's list
// names, reads every acme/priv* repository, acme/privparent among them through PRIV-3's parent, and
// acme/both and acme/oldname; carol, on PRIV's and PRIV2's lists, reads all nine. With every project
// public, dave, a restricted member no list names, reads none of them (R2/R3 in every state). The
// rename row shows the rule reads no rename: alice reads acme/newname and not acme/oldname, and bob
// reads acme/oldname and not acme/newname. Each readable repository is read through a pull request path
// and a check path, and each hidden one through both too.
//
// Four controls, each run on its own, and each time the test fails naming the repository: show
// every repository to an open viewer (alice reads acme/priv); count open issues only (alice reads
// acme/privdone); resolve an issue's own repository only (alice reads acme/privdefault and bob loses
// acme/privparent); compare names through refs.CanonicalRepoSQL (bob reads acme/newname and alice
// loses it).
func TestTheGitHubProxyShowsARepositoryByItsIssues(t *testing.T) {
	database := seedProxyGate(t)
	fake := &gateGitHub{installed: proxiedRepositories}
	handler := gateRouter(t, database, fake)
	for _, caller := range []struct {
		login string
		reads []string
	}{
		{"alice@example.com", []string{"acme/pub", "acme/none", "acme/both", "acme/newname"}},
		{"bob@example.com", []string{"acme/priv", "acme/privdone", "acme/privparent", "acme/privdefault", "acme/both", "acme/oldname"}},
		{"carol@example.com", proxiedRepositories},
	} {
		for _, path := range []struct{ name, suffix string }{
			{"pull request", "pulls/1"},
			{"check", "commits/0123abcd/check-runs"},
		} {
			uninstalled := proxyAnswer(t, handler, caller.login, uninstalledRepository, path.suffix)
			if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
				t.Fatalf("%s's %s read of %s, which the App is not installed on, answered %s: the comparison proves nothing",
					caller.login, path.name, uninstalledRepository, uninstalled)
			}
			for _, repository := range proxiedRepositories {
				before := len(fake.repositoryReads())
				got := proxyAnswer(t, handler, caller.login, repository, path.suffix)
				read := len(fake.repositoryReads()) > before
				switch {
				case slices.Contains(caller.reads, repository):
					if want := `200 "" {"repository":"<repo>"}`; got != want || !read {
						t.Errorf("%s reads %s through its %s path as %s (GitHub read: %t), want GitHub's answer %s",
							caller.login, repository, path.name, got, read, want)
					}
				case got != uninstalled || read:
					t.Errorf("%s reads %s, which the person may not see, through its %s path as\n\t%s (GitHub read: %t)\nwant what %s, which the App is not installed on, answers:\n\t%s",
						caller.login, repository, path.name, got, read, uninstalledRepository, uninstalled)
				}
			}
		}
	}

	// With every project public, a restricted member still reads only the repositories an issue of
	// a project whose list names them resolves to: dave, on no list, reads none of the nine, each
	// answered as acme/uninstalled is, with nothing read from GitHub.
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	uninstalled := proxyAnswer(t, handler, "dave@example.com", uninstalledRepository, "pulls/1")
	for _, repository := range proxiedRepositories {
		before := len(fake.repositoryReads())
		got := proxyAnswer(t, handler, "dave@example.com", repository, "pulls/1")
		if read := len(fake.repositoryReads()) > before; got != uninstalled || read {
			t.Errorf("with every project public, dave, a restricted member no list names, reads %s as\n\t%s (GitHub read: %t)\nwant what %s answers:\n\t%s",
				repository, got, read, uninstalledRepository, uninstalled)
		}
	}
}

// seedProxyGate writes the people, projects and issues the proxy gate test reads through: alice and
// carol, full members, bob, a restricted member, and olga, every project's owner; PUB, public;
// PRIV, private, whose list names bob and carol; and PRIV2, private, whose list names carol and not
// bob. PUB-1 names acme/pub; PRIV-1 names acme/priv; PRIV-2 names acme/privdone and is done; PRIV-3
// names none and is a child of PRIV2-1, which names acme/privparent, so only the parent walk gives
// bob that repository; PRIV-4 names none and resolves to PRIV's default repository,
// acme/privdefault; PUB-2 and PRIV-5 both name acme/both; PRIV-6 names acme/oldname. No issue names
// acme/none or acme/newname. seedRepositoryRecords stores the default and the rename.
func seedProxyGate(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into people (email, member_kind) values
			('alice@example.com', 'full'), ('bob@example.com', 'restricted'), ('carol@example.com', 'full'), ('olga@example.com', 'full'),
			('dave@example.com', 'restricted');
		insert into projects (key, name, default_assignee, visibility) values
			('PUB', 'Public', 'olga@example.com', 'public'),
			('PRIV', 'Private', 'olga@example.com', 'private'),
			('PRIV2', 'Private two', 'olga@example.com', 'private');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', 'bob@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PRIV', 'person', 'carol@example.com', '{"kind":"user","id":"olga@example.com"}'),
			('PRIV2', 'person', 'carol@example.com', '{"kind":"user","id":"olga@example.com"}');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-1', 'PUB', 1, 'Names acme/pub', '{"kind":"user","id":"olga@example.com"}', 'a', null, 'acme/pub', 'triage'),
			('PUB-2', 'PUB', 2, 'Names acme/both', '{"kind":"user","id":"olga@example.com"}', 'b', null, 'acme/both', 'triage'),
			('PRIV2-1', 'PRIV2', 1, 'Names acme/privparent', '{"kind":"user","id":"olga@example.com"}', 'a', null, 'acme/privparent', 'triage'),
			('PRIV-1', 'PRIV', 1, 'Names acme/priv', '{"kind":"user","id":"olga@example.com"}', 'a', null, 'acme/priv', 'triage'),
			('PRIV-2', 'PRIV', 2, 'Names acme/privdone', '{"kind":"user","id":"olga@example.com"}', 'b', null, 'acme/privdone', 'done'),
			('PRIV-3', 'PRIV', 3, 'Under PRIV2-1', '{"kind":"user","id":"olga@example.com"}', 'c', 'PRIV2-1', null, 'triage'),
			('PRIV-4', 'PRIV', 4, 'Names none', '{"kind":"user","id":"olga@example.com"}', 'd', null, null, 'triage'),
			('PRIV-5', 'PRIV', 5, 'Names acme/both', '{"kind":"user","id":"olga@example.com"}', 'e', null, 'acme/both', 'triage'),
			('PRIV-6', 'PRIV', 6, 'Names acme/oldname', '{"kind":"user","id":"olga@example.com"}', 'f', null, 'acme/oldname', 'triage');
	`); err != nil {
		t.Fatalf("seed the proxy gate: %v", err)
	}
	seedRepositoryRecords(t, database)
	return database
}

// seedRepositoryRecords records PRIV's default repository, acme/privdefault, and a rename of
// acme/oldname to acme/newname, as this build stores both: rows of repo_projects, the default
// marked is_default and the rename a PRIV row with renamed_to. A build that stores either
// elsewhere changes this function alone.
func seedRepositoryRecords(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, is_default, renamed_to, created_by) values
			('acme/privdefault', 'PRIV', true, null, '{"kind":"user","id":"olga@example.com"}'),
			('acme/oldname', 'PRIV', false, 'acme/newname', '{"kind":"user","id":"olga@example.com"}');
	`); err != nil {
		t.Fatalf("record PRIV's default repository and the rename: %v", err)
	}
}

// gateRouter is a router over database whose GitHub is fake, with the App's key, its callers
// named by the X-Dispatch-User header as a person at the browser.
func gateRouter(t *testing.T, database *store.Store, fake *gateGitHub) http.Handler {
	t.Helper()
	_, pem := githubapptest.Key(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    testSigningKey,
		People:        people,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Store:         database,
		App:           &auth.AppConfig{ClientID: "Iv1.gate", PEM: pem},
		GitHubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: fake.serve}),
		Lifetime:      t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return handler
}

// proxyAnswer reads repos/<repository>/<suffix> through the proxy as login, and answers it as
// "<status> <Retry-After> <body>", with the repository's own name written as <repo>, the one byte
// of an answer two repositories may differ in.
func proxyAnswer(t *testing.T, handler http.Handler, login, repository, suffix string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test/api/github/rest/repos/"+repository+"/"+suffix, nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return strings.ReplaceAll(fmt.Sprintf("%d %q %s", response.Code, response.Header().Get("Retry-After"),
		strings.TrimSpace(response.Body.String())), repository, "<repo>")
}

// gateGitHub is GitHub for the repositories it names installed: the App's installation on each of
// them, a token for that installation, and every read of a repository, answered with the
// repository's name and recorded. Every request it is sent, the installation lookups and token
// mints included, is recorded apart.
type gateGitHub struct {
	installed []string
	mu        sync.Mutex
	reads     []string
	every     []string
}

func (g *gateGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.every = append(g.every, r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/11/access_tokens" {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_gate","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	owner, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	name, path, _ := strings.Cut(rest, "/")
	repository := owner + "/" + name
	if path == "installation" {
		if !slices.Contains(g.installed, repository) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read","checks":"read"}}`)
		return
	}
	g.mu.Lock()
	g.reads = append(g.reads, r.Method+" "+r.URL.RequestURI())
	g.mu.Unlock()
	fmt.Fprintf(w, `{"repository":%q}`, repository)
}

// repositoryReads is every read of a repository beyond the installation lookup and the token mint.
func (g *gateGitHub) repositoryReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reads...)
}

// requests is every request GitHub was sent, the installation lookups and token mints included.
func (g *gateGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.every...)
}
