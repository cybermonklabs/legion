// credential_projects_test.go proves that a credential carries its list of projects: a personal
// token's agent_tokens.projects and a broker token's list reach the one rule of what a caller sees
// (internal/dispatch/access), a mint or an edit lists only projects its caller sees, and a
// credential that lists projects passes on only a part of its own list.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The two projects every test here reads: PUB, public and alice's, and PRIV, carol's, which each
// test makes private. alice is a full member who sees PUB alone once PRIV is private; carol sees
// both.
const (
	publicProject  = "PUB"
	privateProject = "PRIV"
)

// listedToken is what the mint and the edit answer of a token, its list included.
type listedToken struct {
	ID       string   `json:"id"`
	Token    string   `json:"token"`
	Projects []string `json:"projects"`
}

// seedCredentialProjects makes PUB as alice and PRIV as carol, both public.
func seedCredentialProjects(t *testing.T, handler http.Handler, database *store.Store) {
	t.Helper()
	seedPeople(t, database, accessCarol)
	for _, project := range []struct{ key, creator string }{{publicProject, "alice"}, {privateProject, accessCarol}} {
		if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project.key, "name": project.key,
		}, project.creator); created.Code != http.StatusCreated {
			t.Fatalf("create %s as %s: status=%d body=%s", project.key, project.creator, created.Code, created.Body.String())
		}
	}
}

// makePrivate makes project private. This release's route refuses the setting (409
// PRIVATE_PROJECTS_OFF) until the deployment's bus can hold it, so the row is written as that
// write will write it.
func makePrivate(t *testing.T, database *store.Store, project string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, project); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
}

// issueIn files an issue in project as login and answers its key.
func issueIn(t *testing.T, handler http.Handler, login, project string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "An issue of " + project,
	}, login)
	if created.Code != http.StatusCreated {
		t.Fatalf("file an issue in %s as %s: status=%d body=%s", project, login, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// mintBody is the body of a mint of a token named name, naming projects unless it is nil.
func mintBody(name string, projects []string) map[string]any {
	body := map[string]any{"name": name}
	if projects != nil {
		body["projects"] = projects
	}
	return body
}

// mintListed mints login's token named name at the person's own browser, naming projects unless it
// is nil, and fails the test unless the mint answers 201.
func mintListed(t *testing.T, handler http.Handler, login, name string, projects []string) listedToken {
	t.Helper()
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody(name, projects), login)
	if minted.Code != http.StatusCreated {
		t.Fatalf("%s mints %q naming %v: status=%d body=%s", login, name, projects, minted.Code, minted.Body.String())
	}
	return decodeBody[listedToken](t, minted)
}

// bearerOf is a request carrying token as a bearer.
func bearerOf(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

// viewerOf is what request's credential sees, resolved as every read resolves its caller: the
// caller optionalActor decides, and access.Resolve over it once.
func viewerOf(t *testing.T, deps Deps, request *http.Request) model.Viewer {
	t.Helper()
	caller, _, err := directServer(deps).optionalActor(request)
	if err != nil {
		t.Fatalf("resolve the caller: %v", err)
	}
	viewer, err := access.Resolve(request.Context(), deps.Store.Pool, caller)
	if err != nil {
		t.Fatalf("resolve what the caller sees: %v", err)
	}
	return viewer
}

// tokenListOf is the list GET /api/v1/me/agent-tokens answers login's browser for token id, and
// whether the row carries a projects field at all.
func tokenListOf(t *testing.T, handler http.Handler, login, id string) ([]string, bool) {
	t.Helper()
	return tokenListIn(t, "GET /api/v1/me/agent-tokens as "+login,
		dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, login), id)
}

// tokenListIn is the list answer, a 200 list of tokens that call names, gives token id, and whether
// the row carries a projects field at all.
func tokenListIn(t *testing.T, call string, answer *httptest.ResponseRecorder, id string) ([]string, bool) {
	t.Helper()
	if answer.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", call, answer.Code, answer.Body.String())
	}
	for _, row := range decodeBody[[]map[string]json.RawMessage](t, answer) {
		var rowID string
		if err := json.Unmarshal(row["id"], &rowID); err != nil {
			t.Fatalf("%s: decode a token row's id: %v", call, err)
		}
		if rowID != id {
			continue
		}
		raw, present := row["projects"]
		if !present {
			return nil, false
		}
		var projects []string
		if err := json.Unmarshal(raw, &projects); err != nil {
			t.Fatalf("%s: decode token %s's projects %s: %v", call, id, raw, err)
		}
		return projects, true
	}
	t.Fatalf("%s holds no token %s: %s", call, id, answer.Body.String())
	return nil, false
}

// TestAPersonalTokenUsesOnlyItsProjects: a token minted with a list carries it on every request,
// so it sees its list within what its person sees, and it gives another token, by a mint or an
// edit, only a part of that list. A token minted with none answers as every token did before
// lists, with no projects field, and sees the public projects its person sees.
func TestAPersonalTokenUsesOnlyItsProjects(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)

	listed := mintListed(t, handler, accessCarol, "priv-only", []string{privateProject})
	if !slices.Equal(listed.Projects, []string{privateProject}) {
		t.Fatalf("the mint naming PRIV answered projects %v, want [PRIV]", listed.Projects)
	}
	open := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("everything-public", nil), accessCarol)
	if open.Code != http.StatusCreated || strings.Contains(open.Body.String(), `"projects"`) {
		t.Fatalf("a mint naming no projects: status=%d body=%s, want 201 with no projects field", open.Code, open.Body.String())
	}
	unlisted := decodeBody[listedToken](t, open)

	if got := viewerOf(t, deps, bearerOf(listed.Token)); !got.Listed || got.Open || !slices.Equal(got.Sees, []string{privateProject}) {
		t.Fatalf("carol's token listing PRIV sees %+v, want PRIV alone, as a viewer holding a list", got)
	}
	if got := viewerOf(t, deps, bearerOf(unlisted.Token)); got.Listed || !got.Open || !slices.Equal(got.Sees, []string{publicProject}) {
		t.Fatalf("carol's token listing nothing sees %+v, want the public projects carol sees, as an open viewer", got)
	}
	if projects, present := tokenListOf(t, handler, accessCarol, listed.ID); !present || !slices.Equal(projects, []string{privateProject}) {
		t.Fatalf("carol's token list shows projects %v (present %v) for the PRIV token, want [PRIV]", projects, present)
	}
	if projects, present := tokenListOf(t, handler, accessCarol, unlisted.ID); present {
		t.Fatalf("carol's token list shows projects %v for the token listing nothing, want no field", projects)
	}

	// A credential that lists projects gives a token only a part of its own list: no project
	// outside it, and not an empty list, which would see every public project.
	for name, projects := range map[string][]string{"a project outside its list": {publicProject}, "no project at all": {}} {
		before := countRows(t, database, `select count(*) from agent_tokens`)
		wantRefusal(t, agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("child", projects), listed.Token),
			http.StatusBadRequest, "TOKEN_PROJECTS_INPUT", "carol's token listing PRIV", "a mint naming "+name)
		if after := countRows(t, database, `select count(*) from agent_tokens`); after != before {
			t.Fatalf("a refused mint naming %s left %d tokens, want %d", name, after, before)
		}
	}
	inherited := agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("child", nil), listed.Token)
	if inherited.Code != http.StatusCreated || !slices.Equal(decodeBody[listedToken](t, inherited).Projects, []string{privateProject}) {
		t.Fatalf("a mint naming no projects under the PRIV token: status=%d body=%s, want 201 listing PRIV, the minter's own list",
			inherited.Code, inherited.Body.String())
	}

	edit := func(token string, body map[string]any) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+unlisted.ID, body, token)
	}
	wantRefusal(t, edit(listed.Token, map[string]any{"scopes": model.Scopes, "projects": []string{publicProject}}),
		http.StatusBadRequest, "TOKEN_PROJECTS_INPUT", "carol's token listing PRIV", "an edit naming PUB")
	edited := edit(listed.Token, map[string]any{"scopes": model.Scopes, "projects": []string{privateProject}})
	if edited.Code != http.StatusOK || !slices.Equal(decodeBody[listedToken](t, edited).Projects, []string{privateProject}) {
		t.Fatalf("an edit naming PRIV under the PRIV token: status=%d body=%s, want 200 listing PRIV", edited.Code, edited.Body.String())
	}
	if got := viewerOf(t, deps, bearerOf(unlisted.Token)); !got.Listed || !slices.Equal(got.Sees, []string{privateProject}) {
		t.Fatalf("the edited token sees %+v, want PRIV alone", got)
	}
	// An edit that names no projects leaves the token's list as it stands.
	kept := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+unlisted.ID,
		map[string]any{"scopes": []string{model.ScopePerson}}, accessCarol)
	if kept.Code != http.StatusOK || !slices.Equal(decodeBody[listedToken](t, kept).Projects, []string{privateProject}) {
		t.Fatalf("an edit of the scopes alone: status=%d body=%s, want 200 still listing PRIV", kept.Code, kept.Body.String())
	}
}

// TestATokenCannotListAProjectItsPersonCannotSee: a mint or an edit naming a project its caller
// does not see is 400 TOKEN_PROJECTS_INPUT, and nothing is written. A private project the caller
// cannot see is refused exactly as a key no project holds, so the refusal says nothing of it. A
// restricted member sees no public project it is not named on, so it cannot list one either.
func TestATokenCannotListAProjectItsPersonCannotSee(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	markRestricted(t, database, "bob")
	own := mintListed(t, handler, "alice", "laptop", nil)

	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("P%02d", i)
	}
	messages := map[string]string{}
	for _, call := range []struct {
		name, login, method, path string
		body                      map[string]any
	}{
		{"a mint naming PRIV", "alice", http.MethodPost, "/api/v1/me/agent-tokens", mintBody("t", []string{privateProject})},
		{"a mint naming NOPE", "alice", http.MethodPost, "/api/v1/me/agent-tokens", mintBody("t", []string{"NOPE"})},
		{"a mint naming seventeen projects", "alice", http.MethodPost, "/api/v1/me/agent-tokens", mintBody("t", tooMany)},
		{"an edit naming PRIV", "alice", http.MethodPatch, "/api/v1/me/agent-tokens/" + own.ID,
			map[string]any{"scopes": model.Scopes, "projects": []string{privateProject}}},
		{"a restricted member's mint naming PUB", "bob", http.MethodPost, "/api/v1/me/agent-tokens", mintBody("t", []string{publicProject})},
	} {
		before := countRows(t, database, `select count(*) from agent_tokens`)
		response := dispatchRequest(t, handler, call.method, call.path, call.body, call.login)
		wantRefusal(t, response, http.StatusBadRequest, "TOKEN_PROJECTS_INPUT", call.login, call.name)
		messages[call.name] = decodeBody[refusalAnswer](t, response).Error
		if after := countRows(t, database, `select count(*) from agent_tokens`); after != before {
			t.Fatalf("%s left %d tokens, want %d", call.name, after, before)
		}
		if projects, present := tokenListOf(t, handler, "alice", own.ID); present {
			t.Fatalf("%s left alice's token listing %v, want no list", call.name, projects)
		}
	}
	if hidden, missing := messages["a mint naming PRIV"], messages["a mint naming NOPE"]; strings.ReplaceAll(hidden, privateProject, "NOPE") != missing {
		t.Fatalf("a hidden project is refused %q and a missing one %q: the two must read alike", hidden, missing)
	}

	// A project she sees she lists, once however often it is named.
	if minted := mintListed(t, handler, "alice", "pub", []string{publicProject, publicProject}); !slices.Equal(minted.Projects, []string{publicProject}) {
		t.Fatalf("alice's mint naming PUB twice lists %v, want [PUB]", minted.Projects)
	}
}

// TestATokenListedBeforeItsPersonLostTheProjectSeesNothingOfIt: alice's token listing PRIV, minted
// while PRIV was public, keeps its list when PRIV becomes private, and from then on sees nothing:
// the rule reads its list against what alice sees on every request. It passes nothing of PRIV on,
// and alice's own token list no longer names PRIV to her, a key she no longer sees.
func TestATokenListedBeforeItsPersonLostTheProjectSeesNothingOfIt(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	token := mintListed(t, handler, "alice", "priv", []string{privateProject})
	if got := viewerOf(t, deps, bearerOf(token.Token)); !slices.Equal(got.Sees, []string{privateProject}) {
		t.Fatalf("alice's PRIV token sees %+v while PRIV is public, want PRIV", got)
	}

	makePrivate(t, database, privateProject)
	if got := viewerOf(t, deps, bearerOf(token.Token)); !got.Listed || got.Open || len(got.Sees) != 0 {
		t.Fatalf("alice's PRIV token sees %+v once PRIV is private, want nothing, as a viewer holding a list", got)
	}
	var stored []string
	if err := database.Pool.QueryRow(context.Background(), `select projects from agent_tokens where id = $1`, token.ID).Scan(&stored); err != nil {
		t.Fatalf("read alice's PRIV token's row: %v", err)
	}
	if !slices.Equal(stored, []string{privateProject}) {
		t.Fatalf("alice's PRIV token's row lists %v, want [PRIV]: nothing rewrites a token's list", stored)
	}
	if projects, present := tokenListOf(t, handler, "alice", token.ID); present {
		t.Fatalf("GET /api/v1/me/agent-tokens as alice lists her PRIV token's projects as %v, want none: alice no longer sees PRIV", projects)
	}
	for name, projects := range map[string][]string{"naming PRIV": {privateProject}, "inheriting its list": nil} {
		wantRefusal(t, agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("child", projects), token.Token),
			http.StatusBadRequest, "TOKEN_PROJECTS_INPUT", "alice's PRIV token", "a mint "+name)
	}
}

// TestANamedTokenWithNoProjectUsesPublicProjectsOnly: a broker token an operator's entry named,
// whose person is carol, reads PUB and not PRIV, writes a comment on a PUB issue, gets 404 on PRIV's
// issue for a read and a write, and is refused 403 OAUTH_SCOPE_REQUIRED on GET /api/v1/inbox: a
// named token holds no OAuth scope. Each named token the broker mints is held to it: the one naming
// a project, whose list is that project, and the one naming none, whose viewer is open.
func TestANamedTokenWithNoProjectUsesPublicProjectsOnly(t *testing.T) {
	for _, named := range []struct {
		name    string
		project string
		listed  bool
		skip    string
	}{
		{name: "carol's named token naming PUB", project: publicProject, listed: true},
		{name: "carol's named token naming no project",
			skip: "needs PlanD t-d-2: this stack's brokertoken refuses a named token naming no project (Verifier.live answers ErrNotASession)"},
	} {
		t.Run(named.name, func(t *testing.T) {
			if named.skip != "" {
				t.Skip(named.skip)
			}
			rig := newBrokerTokenRig(t)
			seedCredentialProjects(t, rig.handler, rig.database)
			pub := issueIn(t, rig.handler, "alice", publicProject)
			priv := issueIn(t, rig.handler, accessCarol, privateProject)
			makePrivate(t, rig.database, privateProject)
			caller := newBrokerCaller(t)
			const token = "carols-named-token"
			answer := liveNamedToken(caller.jkt)
			answer.Person, answer.Project = accessCarol, named.project
			rig.broker.Says(token, answer)
			serve := func(method, path string, body any) *httptest.ResponseRecorder {
				return rig.serve(caller.presenting(t, method, path, token, body))
			}

			t.Run("GET /api/v1/inbox", func(t *testing.T) {
				inbox := serve(http.MethodGet, "/api/v1/inbox", nil)
				if inbox.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, inbox).Code != codeScopeRequired {
					t.Fatalf("GET /api/v1/inbox under %s = %d %s, want 403 %s: a named token holds no OAuth scope",
						named.name, inbox.Code, inbox.Body.String(), codeScopeRequired)
				}
			})
			t.Run("sees the public projects only", func(t *testing.T) {
				got := viewerOf(t, rig.deps, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
				if !slices.Equal(got.Sees, []string{publicProject}) || got.Listed != named.listed || got.Open == named.listed {
					t.Fatalf("%s sees %+v, want PUB alone, listed %v", named.name, got, named.listed)
				}
			})
			t.Run("reads PUB's issue", func(t *testing.T) {
				if read := serve(http.MethodGet, "/api/v1/issues/"+pub, nil); read.Code != http.StatusOK {
					t.Fatalf("GET /api/v1/issues/%s under %s = %d %s, want 200", pub, named.name, read.Code, read.Body.String())
				}
			})
			t.Run("writes a comment on PUB's issue", func(t *testing.T) {
				if wrote := serve(http.MethodPost, "/api/v1/issues/"+pub+"/comments", map[string]any{"body": "From a named token."}); wrote.Code != http.StatusCreated {
					t.Fatalf("POST /api/v1/issues/%s/comments under %s = %d %s, want 201", pub, named.name, wrote.Code, wrote.Body.String())
				}
			})
			t.Run("reads PRIV's issue as missing", func(t *testing.T) {
				if read := serve(http.MethodGet, "/api/v1/issues/"+priv, nil); read.Code != http.StatusNotFound {
					t.Fatalf("GET /api/v1/issues/%s under %s = %d %s, want 404", priv, named.name, read.Code, read.Body.String())
				}
			})
			t.Run("writes on PRIV's issue as missing", func(t *testing.T) {
				t.Skip("needs t-a-19 (writes)")
				if wrote := serve(http.MethodPost, "/api/v1/issues/"+priv+"/comments", map[string]any{"body": "Never lands."}); wrote.Code != http.StatusNotFound {
					t.Fatalf("POST /api/v1/issues/%s/comments under %s = %d %s, want 404", priv, named.name, wrote.Code, wrote.Body.String())
				}
			})
		})
	}
}

// TestATokenListShowsOnlyTheProjectsItsReaderSees: each token's list, in GET
// /api/v1/me/agent-tokens and in the PATCH answer, holds only the projects the reading credential
// sees, so a credential that sees only public projects learns no private project's key from a
// sibling token (R3). Carol's browser, whose person sees PRIV, still reads every list whole, and
// what a token lists is unchanged by who reads it. With no private project, a token reads the list
// byte for byte as the browser does (R9).
func TestATokenListShowsOnlyTheProjectsItsReaderSees(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	first := mintListed(t, handler, accessCarol, "first", []string{privateProject})
	second := mintListed(t, handler, accessCarol, "second", []string{privateProject, publicProject})
	open := mintListed(t, handler, accessCarol, "open", nil)
	const route = "GET /api/v1/me/agent-tokens"
	asToken := func(token string) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, token)
	}
	asCarol := func() *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, accessCarol)
	}

	t.Run("with no private project a token reads the list as the browser does", func(t *testing.T) {
		token := asToken(open.Token).Body.String()
		if browser := asCarol().Body.String(); token != browser {
			t.Fatalf("%s under carol's token listing nothing = %s, want carol's browser's answer byte for byte: %s", route, token, browser)
		}
		if !strings.Contains(token, `"projects":["PRIV"]`) {
			t.Fatalf("%s under carol's token listing nothing = %s, want first's list [PRIV], which it sees while PRIV is public", route, token)
		}
	})

	makePrivate(t, database, privateProject)
	for _, read := range []struct {
		name          string
		answer        func() *httptest.ResponseRecorder
		first, second []string
	}{
		{"carol's browser", asCarol, []string{privateProject}, []string{privateProject, publicProject}},
		{"carol's token listing nothing", func() *httptest.ResponseRecorder { return asToken(open.Token) }, nil, []string{publicProject}},
		{"carol's token listing PRIV", func() *httptest.ResponseRecorder { return asToken(first.Token) }, []string{privateProject}, []string{privateProject}},
	} {
		t.Run(read.name+" reads only the projects it sees", func(t *testing.T) {
			answer := read.answer()
			call := route + " under " + read.name
			for _, token := range []struct {
				name, id string
				want     []string
			}{{"first", first.ID, read.first}, {"second", second.ID, read.second}, {"open", open.ID, nil}} {
				if got, present := tokenListIn(t, call, answer, token.id); !slices.Equal(got, token.want) || present != (token.want != nil) {
					t.Fatalf("%s lists %s's projects as %v (present %v), want %v", call, token.name, got, present, token.want)
				}
			}
			if read.first == nil && strings.Contains(answer.Body.String(), privateProject) {
				t.Fatalf("%s names PRIV, which it does not see: %s", call, answer.Body.String())
			}
		})
	}

	t.Run("the PATCH answer leaves it out, and the token keeps its list", func(t *testing.T) {
		edited := agentRequest(t, handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+first.ID,
			map[string]any{"scopes": model.Scopes}, open.Token)
		if edited.Code != http.StatusOK || strings.Contains(edited.Body.String(), privateProject) {
			t.Fatalf("PATCH /api/v1/me/agent-tokens/{id} under carol's token listing nothing = %d %s, want 200 naming no PRIV",
				edited.Code, edited.Body.String())
		}
		if got, present := tokenListOf(t, handler, accessCarol, first.ID); !present || !slices.Equal(got, []string{privateProject}) {
			t.Fatalf("first lists %v (present %v) after the edit, want [PRIV]: a reader's cut never reaches the row", got, present)
		}
	})
}
