package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// projectRepositoriesPath is the repository answer's path for project key.
func projectRepositoriesPath(key string) string {
	return "/api/v1/access/projects/" + key + "/repositories"
}

// seedAnswerProject writes project key, public, and one root issue for each repository named,
// numbered from 1 in that order: an empty name is an issue that names none.
func seedAnswerProject(t *testing.T, database *store.Store, key string, repositories ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`, key); err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
	for i, repository := range repositories {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, repo)
			values ($1 || '-' || $2::int, $1, $2::int, 'An issue', '{"kind":"user","id":"alice"}', lpad($2::text, 8, '0'), nullif($3, ''))
		`, key, i+1, repository); err != nil {
			t.Fatalf("write %s-%d naming %q: %v", key, i+1, repository, err)
		}
	}
}

// TestTheRepositoryAnswerAdmitsOnlyTheAccessAccounts: Dispatch's repository answer, GET
// /api/v1/access/projects/{key}/repositories, is answered only to a service account
// DISPATCH_ACCESS_ACCOUNTS names. A person's cookie, that person's personal token and an outside
// agent's broker token naming the project are each 403 ACCESS_ACCOUNT_REQUIRED, as are a verified
// service account the list does not name and the shared token; a caller with no credential is 401
// NO_IDENTITY. Each refusal names the route, as every access route's does. Each of the refused
// credentials is first shown live, answered 200 by GET /api/v1/whoami, so each 403 is this route's
// refusal and not a credential that resolves nowhere. Control: the listed account is answered 200
// with the project's repositories.
func TestTheRepositoryAnswerAdmitsOnlyTheAccessAccounts(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	rig := newBrokerTokenRigOn(t, testServerOptions{
		store: database, oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount},
		identity: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
	})
	seedAnswerProject(t, database, "PRIV", "acme/pub")
	makePrivate(t, database, "PRIV")
	path := projectRepositoriesPath("PRIV")
	serviceToken := func(subject string) string {
		return issuer.Mint(t, issuer.signing, issuer.Claims(subject, serviceTokenAudience))
	}

	listed := agentRequest(t, rig.handler, http.MethodGet, path, nil, serviceToken(listedServiceAccount))
	if want := `{"project":"PRIV","repositories":["acme/pub"]}` + "\n"; listed.Code != http.StatusOK || listed.Body.String() != want {
		t.Fatalf("GET %s as %s = %d %s, want 200 %s", path, listedServiceAccount, listed.Code, listed.Body.String(), want)
	}

	generation, err := sessions.EnsureSession(context.Background(), "alice")
	if err != nil {
		t.Fatalf("establish alice's session: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice", generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse alice's session cookie: %v", err)
	}
	withCookie := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.AddCookie(cookie)
		return rig.serve(request)
	}
	personal := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	agent := newBrokerCaller(t)
	const brokerToken = "an-outside-agents-token"
	named := liveNamedToken(agent.jkt)
	named.Projects = []string{"PRIV"}
	rig.broker.Says(brokerToken, named)

	for _, caller := range []struct {
		name string
		send func(target string) *httptest.ResponseRecorder
	}{
		{"alice's cookie", withCookie},
		{"alice's personal token", func(target string) *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, target, nil, personal)
		}},
		{"an outside agent's broker token naming PRIV", func(target string) *httptest.ResponseRecorder {
			return rig.serve(agent.presenting(t, http.MethodGet, target, brokerToken, nil))
		}},
		{"a service account the list does not name", func(target string) *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, target, nil, serviceToken(unlistedServiceAccount))
		}},
		{"the shared token", func(target string) *httptest.ResponseRecorder {
			return agentRequest(t, rig.handler, http.MethodGet, target, nil, "agent-token")
		}},
	} {
		if live := caller.send("/api/v1/whoami"); live.Code != http.StatusOK {
			t.Errorf("GET /api/v1/whoami as %s = %d %s, want 200: the credential must be live for its refusal below to be the route's",
				caller.name, live.Code, live.Body.String())
			continue
		}
		response := caller.send(path)
		if body := response.Body.String(); response.Code != http.StatusForbidden || !strings.Contains(body, `"code":"ACCESS_ACCOUNT_REQUIRED"`) ||
			!strings.Contains(body, "GET /api/v1/access/projects/{key}/repositories is read only by a service account this deployment names in DISPATCH_ACCESS_ACCOUNTS") ||
			strings.Contains(body, "acme/") {
			t.Errorf("GET %s as %s = %d %s, want 403 ACCESS_ACCOUNT_REQUIRED, naming the route as the listed accounts' and naming no repository",
				path, caller.name, response.Code, body)
		}
	}
	wantRefusal(t, dispatchRequest(t, rig.handler, http.MethodGet, path, nil, ""),
		http.StatusUnauthorized, "NO_IDENTITY", "a caller with no credential", "GET "+path)
}

// TestADeepChainIsUnresolvedNotShort: the repository answer of a project one of whose issues sits
// 33 links below its root, past the walk's 32 (refs.IssueDepthCap), is 503 REPOSITORIES_UNRESOLVED
// naming that issue, DEEP-34, and no repository: never the list of what the walk could resolve,
// since the chain's unread ancestors may name a repository the list would leave out. The chain's
// root names acme/deeproot and the project's default is acme/deepdefault. With DEEP-34 deleted,
// DEEP-33, 32 links below the root, resolves, and the answer is 200 with acme/deeproot alone, so
// the refusal is the cap's own boundary and not a fixture that fails for every issue. The refusal
// also logs one WARN line naming the project and the issue, since the 503 reaches only the secrets
// broker, and the 200 logs none. Controls: drop refs.ProjectRepositories' refusal of the unresolved
// issue, and the answer is 200 with the list of the others and the test fails; drop the WARN line,
// and the test fails naming the line it wants.
func TestADeepChainIsUnresolvedNotShort(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DEEP', 'Deep', 'alice');
		insert into repo_projects (repo, project, is_default, created_by) values ('acme/deepdefault', 'DEEP', true, '{"kind":"user","id":"alice"}');
		insert into issues (key, project_key, number, title, parent_key, repo, created_by, rank)
		select 'DEEP-' || n, 'DEEP', n, 'Chain ' || n,
		       case when n = 1 then null else 'DEEP-' || (n - 1) end,
		       case when n = 1 then 'acme/deeproot' end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, 34) n
		order by n;
	`); err != nil {
		t.Fatalf("write a chain of 33 links: %v", err)
	}
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))
	path := projectRepositoriesPath("DEEP")
	logged := captureLog(t)
	unresolvedMessage := `level=WARN msg="dispatch: the repository answer is unresolved"`
	unresolvedLine := unresolvedMessage + ` project=DEEP issue=DEEP-34`

	response := agentRequest(t, handler, http.MethodGet, path, nil, bearer)
	body := response.Body.String()
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(body, `"code":"REPOSITORIES_UNRESOLVED"`) ||
		!strings.Contains(body, "DEEP-34") || strings.Contains(body, "acme/") || strings.Contains(body, `"repositories"`) {
		t.Fatalf("GET %s over a chain of 33 links = %d %s, want 503 REPOSITORIES_UNRESOLVED naming DEEP-34 and no repository", path, response.Code, body)
	}
	if got := strings.Count(logged.String(), unresolvedLine); got != 1 {
		t.Fatalf("the 503 logged %d lines %q, want 1 in:\n%s", got, unresolvedLine, logged.String())
	}

	if _, err := database.Pool.Exec(ctx, `delete from issues where key = 'DEEP-34'`); err != nil {
		t.Fatalf("delete DEEP-34: %v", err)
	}
	if response := agentRequest(t, handler, http.MethodGet, path, nil, bearer); response.Code != http.StatusOK ||
		response.Body.String() != `{"project":"DEEP","repositories":["acme/deeproot"]}`+"\n" {
		t.Fatalf("GET %s over a chain of 32 links = %d %s, want 200 with acme/deeproot alone", path, response.Code, response.Body.String())
	}
	if got := strings.Count(logged.String(), unresolvedMessage); got != 1 {
		t.Fatalf("after the 200 the log holds %d lines %q, want the 503's one alone:\n%s", got, unresolvedMessage, logged.String())
	}
}

// TestTheRepositoryAnswerIsSortedOnceAndNeverNull: the answer is {project, repositories},
// the repositories sorted byte by byte, as Go's sort.Strings orders them, and each once, whatever
// order the issues name them in and however many name one; a project none of whose issues resolves
// to a repository answers repositories [], never null or left out; an unknown key is 404
// PROJECT_NOT_FOUND. The names are ones a collation that skips punctuation orders otherwise
// (acme/aa before acme/a-b), so only a byte-wise order passes.
func TestTheRepositoryAnswerIsSortedOnceAndNeverNull(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t), accessAccounts: []string{listedServiceAccount}})
	seedAnswerProject(t, database, "SHAPE", "acme/aa", "acme/a_b", "acme/aa", "acme/a-b", "acme/a.b", "")
	seedAnswerProject(t, database, "EMPTY", "")
	bearer := issuer.Mint(t, issuer.signing, issuer.Claims(listedServiceAccount, serviceTokenAudience))

	for _, row := range []struct{ project, want string }{
		{"SHAPE", `{"project":"SHAPE","repositories":["acme/a-b","acme/a.b","acme/a_b","acme/aa"]}` + "\n"},
		{"EMPTY", `{"project":"EMPTY","repositories":[]}` + "\n"},
	} {
		if response := agentRequest(t, handler, http.MethodGet, projectRepositoriesPath(row.project), nil, bearer); response.Code != http.StatusOK ||
			response.Body.String() != row.want {
			t.Errorf("GET %s = %d %s, want 200 %s", projectRepositoriesPath(row.project), response.Code, response.Body.String(), row.want)
		}
	}
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, projectRepositoriesPath("NOPE"), nil, bearer),
		http.StatusNotFound, "PROJECT_NOT_FOUND", listedServiceAccount, "GET "+projectRepositoriesPath("NOPE"))
}
