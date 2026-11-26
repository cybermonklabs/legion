package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two projects and the three people of verification v16's seed (the private-projects
// live-verification.md): PUB is public and PRIV private; alice is a full member no access list
// names, bob a restricted member PRIV's list names, and carol a full member PRIV's list names.
// carol made both projects, so she owns both. bob and carol are emails because an access list
// names a person by email (access.PersonPrincipal); alice is the test server's own "alice".
//
// This slice carries the seed's reads, so three of its writes are held back, each with its row.
// PRIV's access list is filled through the code the route calls (access.AddPrincipal), because
// the route that names a person on it is the access routes' (S2:
// PUT /api/v1/projects/{key}/access/people/{email}, which answers 404 here); S2 restores the
// route call. bob is a full member, because the write that marks a person restricted is that
// slice's too (markRestricted). And a caller acts as a person at a browser or through a personal
// token, never as a session, because a session's own credential is the broker's
// (stubBrokerTokens, S7).
//
// The seed's rule, which holds from S2: a caller sees a project when their person sees it (the
// membership answer) and their credential lists no projects or lists it. At this head every
// caller is answered both projects by GET /api/v1/projects, PRIV included, with bob and carol
// both on PRIV's list: the read binds an open viewer while the deployment holds no restricted
// member, which is this slice's own premise.
const (
	seedPublic  = "PUB"
	seedPrivate = "PRIV"
	seedAlice   = "alice"
	seedBob     = "bob@outside.example"
	seedCarol   = "carol@example.com"
)

// privateCaller is one caller of verification v16, by the name a failure prints.
type privateCaller string

// The eight callers of verification v16 with plan A v87's #t-a-22: three people at a browser, four
// personal tokens and the deployment's shared agent token. A full member's token that lists a
// project sees every public project its person sees and the private projects it lists
// (access.CredentialCovers), so the confined token is bob's, a restricted member's, which lists
// PRIV.
const (
	callerAlice          privateCaller = "alice"
	callerBob            privateCaller = "bob"
	callerCarol          privateCaller = "carol"
	callerCarolPrivToken privateCaller = "carol's token that lists PRIV"
	callerAlicePrivToken privateCaller = "alice's token that lists PRIV"
	callerBobPrivToken   privateCaller = "bob's token that lists PRIV"
	callerCarolToken     privateCaller = "carol's token that lists no project"
	callerSharedToken    privateCaller = "the shared token"
)

// privateSeed is verification v16's seed on a running test server, written through the server's
// own routes wherever a route can write it: each project and its issue are created, each access
// list is filled, and each personal token is minted with its list while PRIV is still public, so
// alice, who sees it then, may name it, as a person at the dashboard would. A test adds the objects
// it reads through the routes too, as one of the callers (request), so what it reads is what the
// server wrote rather than a copy of it.
//
// One fact no route writes here is written beside them, through the code its route calls: PRIV is
// made private by access.SetVisibility, because the visibility route refuses private (409
// PRIVATE_PROJECTS_OFF) until a deployment allows it. A token's list reaches its credential where
// the server reads its agent_tokens.projects (personalTokenActor).
type privateSeed struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	server   *server
	// issue is the one issue of each project, by project key, which carol created.
	issue map[string]string
	// tokens are the plaintext personal tokens, by the caller each one is.
	tokens map[privateCaller]string
}

// seedPrivateProject stands up a test server holding verification v16's seed.
func seedPrivateProject(t *testing.T) privateSeed {
	t.Helper()
	return seedPrivateProjectWith(t, testServerOptions{})
}

// seedPrivateProjectWith is seedPrivateProject on a test server built from options.
func seedPrivateProjectWith(t *testing.T, options testServerOptions) privateSeed {
	t.Helper()
	seed := seedPublicProjectsWith(t, options)
	seed.makePrivate(t, seedPrivate)
	return seed
}

// seedPublicProjectsWith is verification v16's seed on a test server built from options, with both
// projects still public: a test that makes PRIV private through the visibility route itself starts
// from it.
func seedPublicProjectsWith(t *testing.T, options testServerOptions) privateSeed {
	t.Helper()
	handler, database, deps, served := newRegisteredTestServer(t, options)
	seed := privateSeed{
		handler: handler, database: database, deps: deps, server: served,
		issue: map[string]string{}, tokens: map[privateCaller]string{},
	}
	seedPeople(t, database, seedBob, seedCarol)
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": "Project " + project,
		}, http.StatusCreated)
		created := seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "The issue of " + project,
		}, http.StatusCreated)
		seed.issue[project] = decodeBody[struct {
			Key string `json:"key"`
		}](t, created).Key
	}
	for _, person := range []string{seedBob, seedCarol} {
		seed.addToAccessList(t, seedPrivate, person)
	}
	for _, token := range []struct {
		caller   privateCaller
		login    string
		projects []string
	}{
		{callerCarolPrivToken, seedCarol, []string{seedPrivate}},
		{callerAlicePrivToken, seedAlice, []string{seedPrivate}},
		{callerBobPrivToken, seedBob, []string{seedPrivate}},
		{callerCarolToken, seedCarol, []string{}},
	} {
		minted := decodeBody[agentTokenResponse](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens",
			seedMintBody(string(token.caller), token.projects), token.login))
		if minted.Token == "" {
			t.Fatalf("mint %s: no token in the answer", token.caller)
		}
		seed.tokens[token.caller] = minted.Token
	}
	return seed
}

// addToAccessList names person on project key's access list as its owner carol, through the write
// the access route makes (access.AddPrincipal). The route itself,
// PUT /api/v1/projects/{key}/access/people/{email}, is the access routes' (S2), which answers 404
// at this slice.
func (s privateSeed) addToAccessList(t *testing.T, key, person string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin naming %s on %s's access list: %v", person, key, err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.AddPrincipal(ctx, tx, s.deps.Events, key, "person", person, model.Actor{Kind: "user", ID: seedCarol}); err != nil {
		t.Fatalf("name %s on %s's access list: %v", person, key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit naming %s on %s's access list: %v", person, key, err)
	}
}

// seedMintBody is the body that mints a personal token named name listing projects, which
// credential_projects_test.go's mintBody writes once S3c-issues carries that file; this slice has
// the seed and not the file.
func seedMintBody(name string, projects []string) map[string]any {
	body := map[string]any{"name": name}
	if projects != nil {
		body["projects"] = projects
	}
	return body
}

// makePrivate makes project key private as its owner carol, through the write the visibility
// route makes, after the fill marker the switch requires: a deployment holding a private project
// has filled session_projects, since the switch refuses until it has.
func (s privateSeed) makePrivate(t *testing.T, key string) {
	t.Helper()
	if _, err := s.database.Pool.Exec(t.Context(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker before making %s private: %v", key, err)
	}
	s.setVisibility(t, key, model.ProjectPrivate)
}

// setVisibility sets project key's visibility as its owner carol, through the write the
// visibility route makes.
func (s privateSeed) setVisibility(t *testing.T, key, visibility string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin making %s %s: %v", key, visibility, err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.SetVisibility(ctx, tx, s.deps.Events, key, visibility, model.Actor{Kind: "user", ID: seedCarol}); err != nil {
		t.Fatalf("make %s %s: %v", key, visibility, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit making %s %s: %v", key, visibility, err)
	}
}

// login is the person at the browser a caller is, and empty for a caller that presents a token.
func (caller privateCaller) login() string {
	switch caller {
	case callerAlice:
		return seedAlice
	case callerBob:
		return seedBob
	case callerCarol:
		return seedCarol
	}
	return ""
}

// request makes one request to the seeded server as caller: a person at a browser by the test
// identity's header, a personal token or the shared token by its bearer. body is encoded as JSON;
// nil sends none.
func (s privateSeed) request(t *testing.T, caller privateCaller, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return s.requestIn(t, context.Background(), caller, method, target, body)
}

// requestIn is request on ctx, so a tracer that watches a context (recordingKey) records it.
func (s privateSeed) requestIn(t *testing.T, ctx context.Context, caller privateCaller, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the body %s sends to %s %s: %v", caller, method, target, err)
		}
		payload = encoded
	}
	request := httptest.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if login := caller.login(); login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer "+s.bearer(t, caller))
	}
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	return response
}

// mustRequest is request, failing the test unless the answer is status.
func (s privateSeed) mustRequest(t *testing.T, caller privateCaller, method, target string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	response := s.request(t, caller, method, target, body)
	if response.Code != status {
		t.Fatalf("%s: %s %s = %d %s, want %d", caller, method, target, response.Code, response.Body.String(), status)
	}
	return response
}

// bearer is the token a token caller presents.
func (s privateSeed) bearer(t *testing.T, caller privateCaller) string {
	t.Helper()
	if caller == callerSharedToken {
		return "agent-token"
	}
	token, ok := s.tokens[caller]
	if !ok {
		t.Fatalf("the seed holds no token for %s", caller)
	}
	return token
}
