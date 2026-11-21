package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// repositoryIdentity is what GitHub answers for one name: the id and the full name of the
// repository the name reaches now.
type repositoryIdentity struct {
	id       int64
	fullName string
}

// idGitHub is GitHub for the repository ids a write records: the App's installation on every name
// it holds, a token for it, and each name's repository itself, answered with the id and full name
// names holds for it (GET /repos/{owner}/{repo}), as GitHub answers a renamed or transferred
// repository's old name with the repository it moved to. failing answers every id read 502. Every
// id read is counted (idReads). Its names may change while a test runs, under its lock.
type idGitHub struct {
	mu      sync.Mutex
	names   map[string]repositoryIdentity
	failing bool
	idReads []string
}

func newIDGitHub(names map[string]repositoryIdentity) *idGitHub {
	return &idGitHub{names: names}
}

// options is options with this GitHub as the server's App.
func (g *idGitHub) options(t *testing.T, options testServerOptions) testServerOptions {
	t.Helper()
	options.app = fakeGitHubAppConfig(t)
	options.githubAPIBase = githubtest.Server(t, githubtest.Spec{Handle: g.serve})
	return options
}

// set makes GitHub answer name with id and fullName.
func (g *idGitHub) set(name string, id int64, fullName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.names[strings.ToLower(name)] = repositoryIdentity{id: id, fullName: fullName}
}

// fail makes every id read answer 502 (true) or as names holds (false).
func (g *idGitHub) fail(failing bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failing = failing
}

// reads is how many id reads GitHub has answered.
func (g *idGitHub) reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.idReads)
}

func (g *idGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_ids","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	owner, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	name, path, _ := strings.Cut(rest, "/")
	answer, held := g.names[strings.ToLower(owner+"/"+name)]
	switch {
	case path == "installation" && held:
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"metadata":"read"}}`)
	case path == "" && r.Method == http.MethodGet:
		g.idReads = append(g.idReads, owner+"/"+name)
		switch {
		case g.failing:
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		case !held:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			fmt.Fprintf(w, `{"id":%d,"full_name":%q}`, answer.id, answer.fullName)
		}
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// issueRepoID is the GitHub id issue key's own repository holds.
func issueRepoID(t *testing.T, database *store.Store, key string) *int64 {
	t.Helper()
	var id *int64
	if err := database.Pool.QueryRow(context.Background(), `select repo_id from issues where key = $1`, key).Scan(&id); err != nil {
		t.Fatalf("read %s's repository id: %v", key, err)
	}
	return id
}

// defaultRepoID is the GitHub id project's default holds, and whether it has a default.
func defaultRepoID(t *testing.T, database *store.Store, project string) (*int64, bool) {
	t.Helper()
	var id *int64
	err := database.Pool.QueryRow(context.Background(), `select repo_id from repo_projects where project = $1 and is_default`, project).Scan(&id)
	if err != nil {
		return nil, false
	}
	return id, true
}

// idOf answers an id as text, "none" for no id.
func idOf(id *int64) string {
	if id == nil {
		return "none"
	}
	return fmt.Sprint(*id)
}

// shownBy is whether the GitHub proxy shows repository, whose GitHub id is id, to login at the
// browser: routes' repositoryHidden, the cookie's viewer (access.Resolve) held to
// refs.RepositoryClause in the statement the proxy runs.
func shownBy(t *testing.T, database *store.Store, login, repository string, id int64) bool {
	t.Helper()
	viewer, err := access.Resolve(context.Background(), database.Pool, model.BrowserActor(login))
	if err != nil {
		t.Fatalf("resolve %s: %v", login, err)
	}
	var shown bool
	if err := database.Pool.QueryRow(context.Background(), `select `+refs.RepositoryClause("$2::text", "$3::bigint", "$1"),
		refs.VisibilityArg(model.Actor{Viewer: &viewer}), repository, id).Scan(&shown); err != nil {
		t.Fatalf("read whether %s sees %s: %v", login, repository, err)
	}
	return shown
}

// makeProjectPrivate sets project's visibility in its row, as this change's tests seed a private
// project; the switch's own rows are TestASwitchWaitsForTheOwnersRepositoryConfirmation's.
func makeProjectPrivate(t *testing.T, database *store.Store, project string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, project); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
}

// makeNoProjectPrivate makes every project of database public, newPrivateProjectsServer's HIDE
// among them, so the deployment holds no private project (R9) until a test makes one private.
func makeNoProjectPrivate(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'public' where visibility = 'private'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
}

// requireStatus fails unless response answers status, naming what made it.
func requireStatus(t *testing.T, what string, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s: %d %s, want %d", what, response.Code, strings.TrimSpace(response.Body.String()), status)
	}
}

// TestEveryRepositoryWriteRecordsItsID (c-proxy-by-repository-id): while a project is private, each
// write that sets a repository name records the GitHub id GitHub answers for that name as the write
// runs, a write GitHub does not answer is refused, and a write that carries the name its row stores
// keeps the row's id, or its lack of one, and asks GitHub nothing. Over newPrivateProjectsServer's
// projects, with PRIV made private through its row and OTHER public: alice owns both; carol, a full
// member, sees OTHER and not PRIV.
//
//   - With no project private, alice's create of an OTHER issue naming acme/pubearly asks GitHub
//     nothing and stores the name with no id, as before any project could be private (R9).
//   - A PRIV issue's create naming acme/priv records 9101, a patch to acme/privpatched records 9102,
//     OTHER's default write of acme/pubdefault records 9104, and PRIV's of acme/privdefault 9103.
//   - A PRIV issue names acme/privold2, which GitHub answers with acme/privnew2 (9003), and records
//     9003. After a new repository (9010) takes the name, a patch carrying acme/privold2 keeps
//     9003, and GitHub is asked nothing.
//   - An OTHER issue written before ids were recorded names acme/privnew2 with none. After PRIV's
//     owner confirms a PRIV issue's acme/privold2 as 9003 with shared false, carol's patch carrying
//     acme/privnew2 keeps it with no id, asking GitHub nothing, and the proxy still hides
//     acme/privnew2 from carol.
//   - While GitHub does not answer, a project being private: carol's create of an OTHER issue naming
//     acme/pubfail, alice's create of a PRIV issue naming acme/privfail, her patch of a PRIV issue
//     to acme/privfail, and her PRIV default write of acme/privfaildefault each answer 503
//     REPOSITORY_ID_UNAVAILABLE ("GitHub did not answer; try again", naming no project) and write
//     nothing.
//   - Where GitHub answers that Dispatch's App is not installed on acme/notinstalled, alice's create
//     of a PRIV issue naming it, her patch of a PRIV issue to it, and her PRIV default write of it
//     each answer 403 REPOSITORY_NOT_AVAILABLE, byte for byte what carol's create of an OTHER issue
//     naming acme/privnew2, hidden from her, answers, and write nothing.
//   - An OTHER parent above a PRIV child (rows only a direct write makes): clearing the parent's
//     repository, and moving the parent under an OTHER issue whose repository holds no id, are each
//     written, and carol then reads the repository the PRIV child resolves to, through the parent.
//   - Every route writeColumns names is among these writes.
//
// No route moves an issue into another project (TestEveryWriteDeclaresItsColumns: only the issue
// create writes issues.project_key), so no row moves one.
//
// Controls, each run on its own: drop the id read from the default write (OTHER's default records
// none); read the id again for a stored name (the patch after the reuse records 9010); read the id
// for a stored name that has none (carol's re-saved OTHER issue asks GitHub, and is refused the
// repository PRIV holds by that id); store a name with no id when GitHub does not answer, the path
// before this rule (carol's create of acme/pubfail is written); answer every failed id read 503, a
// not-installed repository included (the three acme/notinstalled writes answer 503). Each time the
// test fails naming it.
func TestEveryRepositoryWriteRecordsItsID(t *testing.T) {
	github := newIDGitHub(map[string]repositoryIdentity{
		"acme/priv": {9101, "acme/priv"}, "acme/privpatched": {9102, "acme/privpatched"}, "acme/privdefault": {9103, "acme/privdefault"},
		// Installed, so a failed read of them is GitHub giving no answer, not GitHub answering that
		// the App cannot read them.
		"acme/pubfail": {9301, "acme/pubfail"}, "acme/privfail": {9302, "acme/privfail"}, "acme/privfaildefault": {9303, "acme/privfaildefault"},
		"acme/pubdefault": {9104, "acme/pubdefault"},
		"acme/privold2":   {9003, "acme/privnew2"}, "acme/privnew2": {9003, "acme/privnew2"},
		"acme/pubparent": {9201, "acme/pubparent"}, "acme/pubx": {9202, "acme/pubx"},
	})
	handler, database := newPrivateProjectsServer(t, github.options(t, testServerOptions{privateProjects: []string{"PRIV"}}), &recordingAdmission{})
	exercised := map[string]bool{}
	send := func(route, target string, body any, login string) *httptest.ResponseRecorder {
		t.Helper()
		exercised[route] = true
		method, _, _ := strings.Cut(route, " ")
		return dispatchRequest(t, handler, method, target, body, login)
	}
	create := func(login string, body map[string]any) string {
		t.Helper()
		response := send("POST /api/v1/issues", "/api/v1/issues", body, login)
		requireStatus(t, fmt.Sprintf("%s creates %v", login, body), response, http.StatusCreated)
		return decodeBody[model.Issue](t, response).Key
	}
	patch := func(login, key string, body map[string]any, status int) {
		t.Helper()
		requireStatus(t, fmt.Sprintf("%s patches %s %v", login, key, body), send("PATCH /api/v1/issues/{key}", "/api/v1/issues/"+key, body, login), status)
	}
	putDefault := func(repo, project string, status int) {
		t.Helper()
		requireStatus(t, fmt.Sprintf("alice makes %s %s's default", repo, project), send("PUT /api/v1/settings/repo-projects/{owner}/{repo}",
			"/api/v1/settings/repo-projects/"+repo, map[string]any{"project": project, "default": true}, "alice"), status)
	}
	unconfirmed := create("alice", map[string]any{"project": "PRIV", "title": "Names acme/privold2 before ids", "force": true})
	// With no project private, newPrivateProjectsServer's HIDE included, a write reads no id (R9).
	makeNoProjectPrivate(t, database)
	early := create("alice", map[string]any{"project": "OTHER", "title": "Names acme/pubearly with no project private", "repo": "acme/pubearly", "force": true})
	if id, asked := issueRepoID(t, database, early), github.reads(); id != nil || asked != 0 {
		t.Errorf("with no project private, %s's create naming acme/pubearly recorded %s and asked GitHub %d times, want no id and GitHub asked nothing", early, idOf(id), asked)
	}
	makeProjectPrivate(t, database, "PRIV")

	written := create("alice", map[string]any{"project": "PRIV", "title": "Names acme/priv", "repo": "acme/priv", "force": true})
	if id := issueRepoID(t, database, written); id == nil || *id != 9101 {
		t.Errorf("a PRIV issue's create naming acme/priv recorded %s, want GitHub's 9101", idOf(id))
	}
	patch("alice", written, map[string]any{"repo": "acme/privpatched"}, http.StatusOK)
	if id := issueRepoID(t, database, written); id == nil || *id != 9102 {
		t.Errorf("a patch of %s to acme/privpatched recorded %s, want GitHub's 9102", written, idOf(id))
	}
	// A public project's default records its id, where no guard would refuse a write with none; then
	// a private project's.
	putDefault("acme/pubdefault", "OTHER", http.StatusOK)
	if id, found := defaultRepoID(t, database, "OTHER"); !found || id == nil || *id != 9104 {
		t.Errorf("OTHER's default write of acme/pubdefault recorded %s (a default: %t), want GitHub's 9104", idOf(id), found)
	}
	putDefault("acme/privdefault", "PRIV", http.StatusOK)
	if id, found := defaultRepoID(t, database, "PRIV"); !found || id == nil || *id != 9103 {
		t.Errorf("PRIV's default write of acme/privdefault recorded %s (a default: %t), want GitHub's 9103", idOf(id), found)
	}

	// An OTHER issue that names acme/privnew2, written while no private project held 9003: the row
	// is made one written before ids below.
	elsewhere := create("carol@example.com", map[string]any{"project": "OTHER", "title": "Names acme/privnew2 before ids", "repo": "acme/privnew2", "force": true})

	// The reuse: a stored name keeps the id it holds, and GitHub is asked nothing.
	old := create("alice", map[string]any{"project": "PRIV", "title": "Names acme/privold2", "repo": "acme/privold2", "force": true})
	if id := issueRepoID(t, database, old); id == nil || *id != 9003 {
		t.Fatalf("a PRIV issue's create naming acme/privold2, which GitHub answers with acme/privnew2, recorded %s, want 9003", idOf(id))
	}
	github.set("acme/privold2", 9010, "acme/privold2")
	before := github.reads()
	patch("alice", old, map[string]any{"repo": "acme/privold2"}, http.StatusOK)
	if id, asked := issueRepoID(t, database, old), github.reads()-before; id == nil || *id != 9003 || asked != 0 {
		t.Errorf("a patch of %s carrying its stored acme/privold2, after 9010 took the name, left %s and asked GitHub %d times, want 9003 kept and GitHub asked nothing",
			old, idOf(id), asked)
	}

	// A name an OTHER issue held before ids, which PRIV's owner did not share.
	for _, statement := range []struct {
		sql string
		key string
	}{
		{`update issues set repo_id = null where key = $1`, elsewhere},
		{`update issues set repo = 'acme/privold2', repo_id = null where key = $1`, unconfirmed},
	} {
		if _, err := database.Pool.Exec(context.Background(), statement.sql, statement.key); err != nil {
			t.Fatalf("write %s as a row written before ids: %v", statement.key, err)
		}
	}
	github.set("acme/privold2", 9003, "acme/privnew2")
	requireStatus(t, "alice confirms PRIV's acme/privold2 as 9003, not shared", send("POST /api/v1/projects/{key}/repositories/confirm",
		"/api/v1/projects/PRIV/repositories/confirm", map[string]any{"repo": "acme/privold2", "id": 9003, "shared": false}, "alice"), http.StatusOK)
	if id := issueRepoID(t, database, unconfirmed); id == nil || *id != 9003 {
		t.Errorf("PRIV's confirmation left %s holding %s, want 9003", unconfirmed, idOf(id))
	}
	before = github.reads()
	resaved := send("PATCH /api/v1/issues/{key}", "/api/v1/issues/"+elsewhere, map[string]any{"repo": "acme/privnew2"}, "carol@example.com")
	if id, asked := issueRepoID(t, database, elsewhere), github.reads()-before; resaved.Code != http.StatusOK || id != nil || asked != 0 {
		t.Errorf("carol's patch of %s carrying its stored acme/privnew2 answered %d %s, left %s and asked GitHub %d times, want 200, no id and GitHub asked nothing",
			elsewhere, resaved.Code, strings.TrimSpace(resaved.Body.String()), idOf(id), asked)
	}
	if shownBy(t, database, "carol@example.com", "acme/privnew2", 9003) {
		t.Errorf("carol, who sees OTHER and not PRIV, reads acme/privnew2 (9003) through the GitHub proxy: PRIV's owner did not share it")
	}

	// GitHub does not answer while a project is private: every repository write is refused.
	github.fail(true)
	issues := countRows(t, database, `select count(*) from issues`)
	requireUnavailable := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		var refusal refusalAnswer
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != http.StatusServiceUnavailable || refusal.Code != codeRepositoryIDUnavailable || refusal.Error != "GitHub did not answer; try again" {
			t.Errorf("%s while GitHub does not answer: %d %s, want 503 %s, GitHub did not answer; try again",
				what, response.Code, strings.TrimSpace(response.Body.String()), codeRepositoryIDUnavailable)
		}
	}
	requireUnavailable("carol's create of an OTHER issue naming acme/pubfail",
		send("POST /api/v1/issues", "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Names acme/pubfail", "repo": "acme/pubfail", "force": true}, "carol@example.com"))
	requireUnavailable("alice's create of a PRIV issue naming acme/privfail",
		send("POST /api/v1/issues", "/api/v1/issues", map[string]any{"project": "PRIV", "title": "Names acme/privfail", "repo": "acme/privfail", "force": true}, "alice"))
	if after := countRows(t, database, `select count(*) from issues`); after != issues {
		t.Errorf("the refused creates left %d issues, want %d", after, issues)
	}
	requireUnavailable("alice's patch of "+written+" to acme/privfail",
		send("PATCH /api/v1/issues/{key}", "/api/v1/issues/"+written, map[string]any{"repo": "acme/privfail"}, "alice"))
	if repo := repoOf(decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+written, nil, "alice")).Repo); repo != "acme/privpatched" {
		t.Errorf("the refused patch left %s naming %s, want acme/privpatched", written, repo)
	}
	requireUnavailable("alice's PRIV default write of acme/privfaildefault",
		send("PUT /api/v1/settings/repo-projects/{owner}/{repo}", "/api/v1/settings/repo-projects/acme/privfaildefault",
			map[string]any{"project": "PRIV", "default": true}, "alice"))
	if mapped := countRows(t, database, `select count(*) from repo_projects where repo = 'acme/privfaildefault'`); mapped != 0 {
		t.Errorf("the refused default write left %d mappings of acme/privfaildefault, want none", mapped)
	}
	github.fail(false)

	// GitHub answers that the App is not installed on acme/notinstalled: refused as a repository
	// hidden from the writer is, in the same bytes, with nothing written.
	hiddenRefusal := send("POST /api/v1/issues", "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Names acme/privnew2", "repo": "acme/privnew2", "force": true}, "carol@example.com")
	hiddenBody := strings.TrimSpace(hiddenRefusal.Body.String())
	if hiddenRefusal.Code != http.StatusForbidden || !strings.Contains(hiddenBody, `"code":"`+codeRepositoryNotAvailable+`"`) {
		t.Fatalf("carol's create of an OTHER issue naming acme/privnew2, which PRIV holds unshared, answered %d %s, want 403 %s: the comparison proves nothing",
			hiddenRefusal.Code, hiddenBody, codeRepositoryNotAvailable)
	}
	requireNotAvailable := func(what string, response *httptest.ResponseRecorder) {
		t.Helper()
		if got := strings.TrimSpace(response.Body.String()); response.Code != http.StatusForbidden || got != hiddenBody {
			t.Errorf("%s names acme/notinstalled, which Dispatch's App cannot read: %d %s, want 403 and the bytes a hidden repository is refused with: %s",
				what, response.Code, got, hiddenBody)
		}
	}
	issues = countRows(t, database, `select count(*) from issues`)
	requireNotAvailable("alice's create of a PRIV issue",
		send("POST /api/v1/issues", "/api/v1/issues", map[string]any{"project": "PRIV", "title": "Names acme/notinstalled", "repo": "acme/notinstalled", "force": true}, "alice"))
	if after := countRows(t, database, `select count(*) from issues`); after != issues {
		t.Errorf("the refused not-installed create left %d issues, want %d", after, issues)
	}
	requireNotAvailable("alice's patch of "+written,
		send("PATCH /api/v1/issues/{key}", "/api/v1/issues/"+written, map[string]any{"repo": "acme/notinstalled"}, "alice"))
	if repo := repoOf(decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+written, nil, "alice")).Repo); repo != "acme/privpatched" {
		t.Errorf("the refused not-installed patch left %s naming %s, want acme/privpatched", written, repo)
	}
	requireNotAvailable("alice's PRIV default write",
		send("PUT /api/v1/settings/repo-projects/{owner}/{repo}", "/api/v1/settings/repo-projects/acme/notinstalled",
			map[string]any{"project": "PRIV", "default": true}, "alice"))
	if mapped := countRows(t, database, `select count(*) from repo_projects where repo = 'acme/notinstalled'`); mapped != 0 {
		t.Errorf("the refused not-installed default write left %d mappings of acme/notinstalled, want none", mapped)
	}

	// An OTHER parent above a PRIV child.
	parent := create("carol@example.com", map[string]any{"project": "OTHER", "title": "The parent of a PRIV child", "repo": "acme/pubparent", "force": true})
	above := create("carol@example.com", map[string]any{"project": "OTHER", "title": "Names acme/pubx before ids", "repo": "acme/pubx", "force": true})
	child := create("alice", map[string]any{"project": "PRIV", "title": "A PRIV child of an OTHER issue", "force": true})
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatalf("put %s under %s: %v", child, parent, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set repo_id = null where key = $1`, above); err != nil {
		t.Fatalf("write %s as a row written before ids: %v", above, err)
	}
	patch("carol@example.com", parent, map[string]any{"repo": nil}, http.StatusOK)
	patch("carol@example.com", parent, map[string]any{"parent": above}, http.StatusOK)
	if resolved, err := refs.IssueRepository(context.Background(), database.Pool, child); err != nil || resolved == nil || *resolved != "acme/pubx" {
		t.Fatalf("%s resolves to %v (%v), want acme/pubx through %s and %s", child, resolved, err, parent, above)
	}
	if !shownBy(t, database, "carol@example.com", "acme/pubx", 9202) {
		t.Errorf("carol, who sees %s, does not read acme/pubx, which it resolves to, through the GitHub proxy", above)
	}

	requireStatus(t, "alice removes PRIV's default mapping", send("DELETE /api/v1/settings/repo-projects/{owner}/{repo}",
		"/api/v1/settings/repo-projects/acme/privdefault", nil, "alice"), http.StatusNoContent)
	for _, route := range slices.Sorted(maps.Keys(writeColumns)) {
		if !exercised[route] {
			t.Errorf("%s declares %v, and this test made no write through it", route, writeColumns[route])
		}
	}
}

// switchToPrivate is PRIV's owner alice's switch of project to private, through the route.
func switchToPrivate(t *testing.T, handler http.Handler, project string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/"+project+"/access", map[string]string{"visibility": model.ProjectPrivate}, "alice")
}

// requireWaitingFor fails unless response is the switch's 409 PRIVATE_PROJECT_NOT_PROVISIONED
// naming exactly names in unconfirmed_repositories, with project left public.
func requireWaitingFor(t *testing.T, database *store.Store, project string, response *httptest.ResponseRecorder, names ...string) {
	t.Helper()
	var refusal unconfirmedRepositoriesRefusal
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusConflict || refusal.Code != "PRIVATE_PROJECT_NOT_PROVISIONED" || !slices.Equal(refusal.UnconfirmedRepositories, names) {
		t.Fatalf("the switch of %s answered %d %s, want 409 PRIVATE_PROJECT_NOT_PROVISIONED naming %q in unconfirmed_repositories",
			project, response.Code, strings.TrimSpace(response.Body.String()), names)
	}
	if visibility := visibilityOf(t, database, project); visibility != model.ProjectPublic {
		t.Fatalf("the refused switch left %s %s, want it public", project, visibility)
	}
}

// TestASwitchWaitsForTheOwnersRepositoryConfirmation: a switch to private waits while a repository
// name its project's own issues hold has no GitHub id. With no project private, PRIV's issues name
// acme/priv and acme/privold2 and their creates read no id (R9). The switch answers 409
// PRIVATE_PROJECT_NOT_PROVISIONED naming both in unconfirmed_repositories, changes nothing and
// appends no event, and OTHER, which the deployment does not provision, answers the same code with
// no such field. PRIV's owner reads acme/privold2 as GitHub's acme/privnew2 (9003) and acme/priv as
// 9101, and confirms acme/privold2: the switch still waits, naming acme/priv alone. Once the owner
// confirms acme/priv too, the switch completes. Control: skip refs.UnconfirmedRepositories, and
// PRIV becomes private with unconfirmed names; the test fails naming them.
func TestASwitchWaitsForTheOwnersRepositoryConfirmation(t *testing.T) {
	github := newIDGitHub(map[string]repositoryIdentity{
		"acme/priv": {9101, "acme/priv"}, "acme/privold2": {9003, "acme/privnew2"},
	})
	handler, database := newPrivateProjectsServer(t, github.options(t, testServerOptions{privateProjects: []string{"PRIV"}}), &recordingAdmission{})
	makeNoProjectPrivate(t, database)
	for _, repo := range []string{"acme/priv", "acme/privold2"} {
		created := createdIssueKey(t, handler, "alice", map[string]any{"project": "PRIV", "title": "Names " + repo, "repo": repo, "force": true})
		if id := issueRepoID(t, database, created); id != nil {
			t.Fatalf("with no project private, %s's create naming %s recorded %s, want none", created, repo, idOf(id))
		}
	}
	before := allEvents(t, database)
	requireWaitingFor(t, database, "PRIV", switchToPrivate(t, handler, "PRIV"), "acme/priv", "acme/privold2")
	if appended := allEvents(t, database) - before; appended != 0 {
		t.Errorf("the refused switch appended %d events, want none", appended)
	}
	unlisted := switchToPrivate(t, handler, "OTHER")
	if unlisted.Code != http.StatusConflict || !strings.Contains(unlisted.Body.String(), `"code":"PRIVATE_PROJECT_NOT_PROVISIONED"`) ||
		strings.Contains(unlisted.Body.String(), "unconfirmed_repositories") {
		t.Errorf("the switch of OTHER, which the deployment does not provision, answered %d %s, want 409 PRIVATE_PROJECT_NOT_PROVISIONED with no unconfirmed_repositories",
			unlisted.Code, strings.TrimSpace(unlisted.Body.String()))
	}

	listed := decodeBody[projectRepositories](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/PRIV/repositories", nil, "alice"))
	index := slices.IndexFunc(listed.Repositories, func(row projectRepository) bool { return row.Repo == "acme/privold2" && row.ID == nil })
	if index < 0 || listed.Repositories[index].GitHubID == nil || *listed.Repositories[index].GitHubID != 9003 ||
		listed.Repositories[index].GitHubFullName == nil || *listed.Repositories[index].GitHubFullName != "acme/privnew2" {
		t.Fatalf("PRIV's owner reads its repositories as %+v, want acme/privold2 with no id, GitHub's acme/privnew2 and 9003", listed.Repositories)
	}
	requireStatus(t, "alice confirms acme/privold2 as 9003", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PRIV/repositories/confirm",
		map[string]any{"repo": "acme/privold2", "id": 9003}, "alice"), http.StatusOK)
	requireWaitingFor(t, database, "PRIV", switchToPrivate(t, handler, "PRIV"), "acme/priv")
	requireStatus(t, "alice confirms acme/priv as 9101", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PRIV/repositories/confirm",
		map[string]any{"repo": "acme/priv", "id": 9101}, "alice"), http.StatusOK)
	startClosing(t, handler, database, "PRIV", "alice")
	made := switchToPrivate(t, handler, "PRIV")
	if made.Code != http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPrivate {
		t.Fatalf("the switch after the owner's confirmations answered %d %s, PRIV %s, want 200 and PRIV private",
			made.Code, strings.TrimSpace(made.Body.String()), visibilityOf(t, database, "PRIV"))
	}
}

// TestARepositoryWriteBesideTheSwitchIsSeen: a repository write and the switch to private of its
// project are ordered by the project's owner lock, so a name a write stores with no id is either
// read by the switch's check or written after the switch, which then reads the project private.
//
// With PRIV public, a write transaction holding PRIV's owner lock shared, as every repository
// write takes it (refuseUnidentifiedRepository), stores PRIV's issue's repository acme/raced with
// no id. PRIV's switch starts beside it and waits; the write commits; the switch answers 409 naming
// acme/raced. And the route's own write takes that lock: while PRIV's owner lock is held
// exclusively, as a switch holds it, a PATCH of PRIV's issue naming acme/racedroute, which reads no
// id while no project is private, waits, and is written, with no id, once the lock is let go.
//
// Controls, each on its own: drop the switch's owner lock (access.LockForAccessWrite's
// lockProject), and the switch completes beside the write; drop the write's shared lock, and the
// PATCH completes while the lock is held. Each time the test fails naming the repository.
func TestARepositoryWriteBesideTheSwitchIsSeen(t *testing.T) {
	github := newIDGitHub(map[string]repositoryIdentity{})
	handler, database := newPrivateProjectsServer(t, github.options(t, testServerOptions{privateProjects: []string{"PRIV"}}), &recordingAdmission{})
	makeNoProjectPrivate(t, database)
	key := createdIssueKey(t, handler, "alice", map[string]any{"project": "PRIV", "title": "Names a repository beside the switch", "force": true})
	ctx := context.Background()
	await := func(answered chan *httptest.ResponseRecorder, what string) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case response := <-answered:
			return response
		case <-time.After(30 * time.Second):
			t.Fatalf("%s did not answer within 30s", what)
			return nil
		}
	}

	write, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the write: %v", err)
	}
	defer write.Rollback(ctx)
	if err := store.LockOwnersShared(ctx, write, store.ProjectOwner("PRIV")); err != nil {
		t.Fatalf("take PRIV's owner lock shared, as a repository write does: %v", err)
	}
	if _, err := write.Exec(ctx, `update issues set repo = 'acme/raced', repo_id = null where key = $1`, key); err != nil {
		t.Fatalf("store %s's repository with no id: %v", key, err)
	}
	switched := make(chan *httptest.ResponseRecorder, 1)
	go func() { switched <- switchToPrivate(t, handler, "PRIV") }()
	// The switch must queue behind this write's own lock, not merely wait on some lock: background
	// work after a create can wait on a lock of its own, which a count of every waiter would take
	// for the switch (storetest.WaitForLockWaiters counts only the holder's).
	if queued := storetest.WaitForLockWaiters(t, write, 1, 10*time.Second); queued < 1 {
		t.Fatalf("PRIV's switch never queued behind the write naming acme/raced, which holds PRIV's owner lock shared")
	}
	select {
	case early := <-switched:
		t.Fatalf("PRIV's switch answered %d %s while a write naming acme/raced with no id held PRIV's lock, want it to wait",
			early.Code, strings.TrimSpace(early.Body.String()))
	default:
	}
	if err := write.Commit(ctx); err != nil {
		t.Fatalf("commit the write: %v", err)
	}
	requireWaitingFor(t, database, "PRIV", await(switched, "the switch"), "acme/raced")

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the switch's stand-in: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.ProjectOwner("PRIV")); err != nil {
		t.Fatalf("hold PRIV's owner lock, as a switch does: %v", err)
	}
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		patched <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"repo": "acme/racedroute"}, "alice")
	}()
	if queued := storetest.WaitForLockWaiters(t, holder, 1, 10*time.Second); queued < 1 {
		t.Fatalf("the PATCH naming acme/racedroute never queued behind PRIV's owner lock, held as a switch holds it")
	}
	select {
	case early := <-patched:
		t.Fatalf("the PATCH naming acme/racedroute answered %d %s while PRIV's owner lock was held, want it to wait",
			early.Code, strings.TrimSpace(early.Body.String()))
	default:
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let PRIV's owner lock go: %v", err)
	}
	requireStatus(t, "the PATCH naming acme/racedroute", await(patched, "the PATCH"), http.StatusOK)
	if id := issueRepoID(t, database, key); id != nil {
		t.Errorf("the PATCH, which read no id while no project was private, recorded %s, want acme/racedroute stored with no id", idOf(id))
	}
}

// TestARepositorySharedBeforeTheSwitchStaysSharedAndIsReported: a repository an issue of another
// project resolves to by its recorded id is shared, and the switch to private neither waits on it
// nor hides it from that issue's people; the owner's list reports it. With HIDE, another project,
// private (newPrivateProjectsServer's seed), so each write reads its id, an OTHER issue and a PRIV
// issue each name acme/both, each recording 9107 at its create. PRIV's switch completes; PRIV's
// owner reads acme/both with shared_elsewhere 1; carol, who sees OTHER and not PRIV, reads
// acme/both through the GitHub proxy.
// Control: make the switch wait on shared_elsewhere as it waits on an unconfirmed name, and the
// switch is refused; the test fails naming acme/both.
func TestARepositorySharedBeforeTheSwitchStaysSharedAndIsReported(t *testing.T) {
	github := newIDGitHub(map[string]repositoryIdentity{"acme/both": {9107, "acme/both"}})
	handler, database := newPrivateProjectsServer(t, github.options(t, testServerOptions{privateProjects: []string{"PRIV"}}), &recordingAdmission{})
	if visibility := visibilityOf(t, database, "HIDE"); visibility != model.ProjectPrivate {
		t.Fatalf("HIDE is %s, want private: a write while no project is private reads no id", visibility)
	}
	for _, project := range []string{"OTHER", "PRIV"} {
		created := createdIssueKey(t, handler, "alice", map[string]any{"project": project, "title": "Works in acme/both", "repo": "acme/both", "force": true})
		if id := issueRepoID(t, database, created); id == nil || *id != 9107 {
			t.Fatalf("%s's create naming acme/both recorded %s, want 9107", created, idOf(id))
		}
	}
	startClosing(t, handler, database, "PRIV", "alice")
	if made := switchToPrivate(t, handler, "PRIV"); made.Code != http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPrivate {
		t.Fatalf("PRIV's switch beside OTHER's issue naming acme/both answered %d %s, want 200 and PRIV private: a shared repository informs and holds nothing",
			made.Code, strings.TrimSpace(made.Body.String()))
	}
	listed := decodeBody[projectRepositories](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/PRIV/repositories", nil, "alice"))
	if len(listed.Repositories) != 1 || listed.Repositories[0].Repo != "acme/both" || listed.Repositories[0].SharedElsewhere != 1 || listed.Repositories[0].NamedElsewhere != 0 {
		t.Errorf("PRIV's owner reads its repositories as %+v, want acme/both alone with shared_elsewhere 1 and named_elsewhere 0", listed.Repositories)
	}
	if !shownBy(t, database, accessCarol, "acme/both", 9107) {
		t.Errorf("carol, who sees OTHER, whose issue names acme/both, does not read acme/both through the GitHub proxy once PRIV is private")
	}
}
