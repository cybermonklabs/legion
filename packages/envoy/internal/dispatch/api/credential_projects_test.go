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

// makePrivate makes project private, naming channels when any are given. This release's route
// refuses the setting (409 PRIVATE_PROJECTS_OFF) until the deployment's bus can hold it, so the row
// is written as that write will write it, with the fill marker the switch requires first: a
// project the route made private implies session_projects_filled, since the switch refuses until
// it is written.
func makePrivate(t *testing.T, database *store.Store, project string, channels ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'private' where key = $1`, project); err != nil {
		t.Fatalf("make %s private: %v", project, err)
	}
	if len(channels) > 0 {
		if _, err := database.Pool.Exec(context.Background(), `update projects set slack_channels = $2 where key = $1`, project, channels); err != nil {
			t.Fatalf("name %s's Slack channels %v: %v", project, channels, err)
		}
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into session_projects_filled select now() where not exists (select 1 from session_projects_filled)`); err != nil {
		t.Fatalf("write the fill marker beside %s: %v", project, err)
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

	if got := viewerOf(t, deps, bearerOf(listed.Token)); len(got.Lists) == 0 || got.Open || !slices.Equal(got.Sees, []string{privateProject}) {
		t.Fatalf("carol's token listing PRIV sees %+v, want PRIV alone, as a viewer holding a list", got)
	}
	if got := viewerOf(t, deps, bearerOf(unlisted.Token)); len(got.Lists) > 0 || !got.Open || !slices.Equal(got.Sees, []string{publicProject}) {
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
	if got := viewerOf(t, deps, bearerOf(unlisted.Token)); len(got.Lists) == 0 || !slices.Equal(got.Sees, []string{privateProject}) {
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
	if got := viewerOf(t, deps, bearerOf(token.Token)); len(got.Lists) == 0 || got.Open || len(got.Sees) != 0 {
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

// TestANamedTokenUsesTheProjectItNames: a broker token an operator's entry named, whose person is
// carol and whose project is PUB, reads PUB and not PRIV, writes a comment on a PUB issue, gets 404
// on PRIV's issue for a read and a write, and is refused 403 OAUTH_SCOPE_REQUIRED on GET
// /api/v1/inbox: a named token holds no OAuth scope (namedTokenReads).
func TestANamedTokenUsesTheProjectItNames(t *testing.T) {
	namedTokenReads(t, []string{publicProject})
}

// TestANamedTokenNamingNoProjectReadsPublicProjectsOnly (c-a-named-no-project-public): a broker
// token an account entry that names no project minted (avenue E's public session pods), whose
// person is carol, reaches Dispatch as carol's token with no list. It is held to public projects
// only, though carol is listed on PRIV: it reads PUB and not PRIV, writes a comment on a PUB issue,
// gets 404 on PRIV's issue for a read and a write, exactly as for a missing one, and is refused
// 403 OAUTH_SCOPE_REQUIRED on GET /api/v1/inbox, a person-scope route (namedTokenReads). The
// broker verifier admits such a token since #4119; before it, it was refused as no session's
// credential, and this test could not run.
func TestANamedTokenNamingNoProjectReadsPublicProjectsOnly(t *testing.T) {
	namedTokenReads(t, nil)
}

// namedTokenReads holds carol's named broker token, naming projects (one project, or none), to
// what a named token reads and writes once PRIV is private.
func namedTokenReads(t *testing.T, projects []string) {
	t.Helper()
	listed := len(projects) > 0
	rig := newBrokerTokenRig(t)
	seedCredentialProjects(t, rig.handler, rig.database)
	pub := issueIn(t, rig.handler, "alice", publicProject)
	priv := issueIn(t, rig.handler, accessCarol, privateProject)
	makePrivate(t, rig.database, privateProject)
	caller := newBrokerCaller(t)
	const token = "carols-named-token"
	answer := liveNamedToken(caller.jkt)
	answer.Person, answer.Projects = accessCarol, projects
	rig.broker.Says(token, answer)
	serve := func(method, path string, body any) *httptest.ResponseRecorder {
		return rig.serve(caller.presenting(t, method, path, token, body))
	}
	name := fmt.Sprintf("carol's named token naming %v", projects)

	t.Run("GET /api/v1/inbox", func(t *testing.T) {
		inbox := serve(http.MethodGet, "/api/v1/inbox", nil)
		if inbox.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, inbox).Code != codeScopeRequired {
			t.Fatalf("GET /api/v1/inbox under %s = %d %s, want 403 %s: a named token holds no OAuth scope",
				name, inbox.Code, inbox.Body.String(), codeScopeRequired)
		}
	})
	t.Run("sees the public projects only", func(t *testing.T) {
		got := viewerOf(t, rig.deps, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		if !slices.Equal(got.Sees, []string{publicProject}) || (len(got.Lists) > 0) != listed || got.Open == listed {
			t.Fatalf("%s sees %+v, want PUB alone, listed %v", name, got, listed)
		}
	})
	t.Run("reads PUB's issue", func(t *testing.T) {
		if read := serve(http.MethodGet, "/api/v1/issues/"+pub, nil); read.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/issues/%s under %s = %d %s, want 200", pub, name, read.Code, read.Body.String())
		}
	})
	t.Run("writes a comment on PUB's issue", func(t *testing.T) {
		if wrote := serve(http.MethodPost, "/api/v1/issues/"+pub+"/comments", map[string]any{"body": "From a named token."}); wrote.Code != http.StatusCreated {
			t.Fatalf("POST /api/v1/issues/%s/comments under %s = %d %s, want 201", pub, name, wrote.Code, wrote.Body.String())
		}
	})
	t.Run("reads PRIV's issue as missing", func(t *testing.T) {
		if read := serve(http.MethodGet, "/api/v1/issues/"+priv, nil); read.Code != http.StatusNotFound {
			t.Fatalf("GET /api/v1/issues/%s under %s = %d %s, want 404: %s is private", priv, name, read.Code, read.Body.String(), privateProject)
		}
	})
	t.Run("writes on PRIV's issue as missing", func(t *testing.T) {
		if wrote := serve(http.MethodPost, "/api/v1/issues/"+priv+"/comments", map[string]any{"body": "Never lands."}); wrote.Code != http.StatusNotFound {
			t.Fatalf("POST /api/v1/issues/%s/comments under %s = %d %s, want 404: %s is private", priv, name, wrote.Code, wrote.Body.String(), privateProject)
		}
	})
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

// agentTokenBeforeHidden is a token answer's row as it was before projects_hidden: model.AgentToken
// with every field it had, in the order it had them. An answer that encodes the same through it is
// byte for byte the answer that release gave.
type agentTokenBeforeHidden struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Scopes     []string `json:"scopes"`
	Projects   []string `json:"projects,omitempty"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	RevokedAt  *string  `json:"revoked_at"`
}

// wantAnswerBeforeHidden fails the test unless body, the JSON answer call gave (one token, or a
// list of them when list is set), is byte for byte what the release before projects_hidden
// answered for the same rows.
func wantAnswerBeforeHidden(t *testing.T, call, body string, list bool) {
	t.Helper()
	var before any
	if list {
		before = &[]agentTokenBeforeHidden{}
	} else {
		before = &agentTokenBeforeHidden{}
	}
	if err := json.Unmarshal([]byte(body), before); err != nil {
		t.Fatalf("%s: decode %s: %v", call, body, err)
	}
	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("%s: encode the answer as before: %v", call, err)
	}
	if want := string(encoded) + "\n"; body != want {
		t.Fatalf("%s = %s, want the answer before projects_hidden byte for byte: %s", call, body, want)
	}
}

// hiddenFlagOf is whether the row of token id in a token answer (a list, or one token) carries
// projects_hidden, and its value.
func hiddenFlagOf(t *testing.T, call, body, id string) (hidden, present bool) {
	t.Helper()
	var rows []map[string]json.RawMessage
	if strings.HasPrefix(body, "[") {
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("%s: decode %s: %v", call, body, err)
		}
	} else {
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &row); err != nil {
			t.Fatalf("%s: decode %s: %v", call, body, err)
		}
		rows = []map[string]json.RawMessage{row}
	}
	for _, row := range rows {
		var rowID string
		if err := json.Unmarshal(row["id"], &rowID); err != nil || rowID != id {
			continue
		}
		raw, present := row["projects_hidden"]
		if !present {
			return false, false
		}
		if err := json.Unmarshal(raw, &hidden); err != nil {
			t.Fatalf("%s: decode token %s's projects_hidden %s: %v", call, id, raw, err)
		}
		return hidden, true
	}
	t.Fatalf("%s holds no token %s: %s", call, id, body)
	return false, false
}

// TestABrowserIsToldItsTokenListsProjectsItCannotSee: a token answer cuts each list to what its
// reader sees (seeToken), so a token whose every project is hidden reads as one that lists
// none. The person's own browser is told so (projects_hidden), so Settings never shows such a token
// as one of the public projects only, nor offers to widen it. A credential reading its person's
// list is never told, since that would say a project exists it cannot see (R3). And with no private
// project, every answer is byte for byte the answer before the field (R9).
func TestABrowserIsToldItsTokenListsProjectsItCannotSee(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	// alice's tokens, minted while PRIV is public and she sees it.
	alicePriv := mintListed(t, handler, "alice", "alice-priv", []string{privateProject})
	aliceBoth := mintListed(t, handler, "alice", "alice-both", []string{privateProject, publicProject})
	alicePub := mintListed(t, handler, "alice", "alice-pub", []string{publicProject})
	aliceOpen := mintListed(t, handler, "alice", "alice-open", nil)
	// carol's, who owns PRIV and sees both projects at her browser.
	carolPriv := mintListed(t, handler, accessCarol, "carol-priv", []string{privateProject})
	carolBoth := mintListed(t, handler, accessCarol, "carol-both", []string{privateProject, publicProject})
	carolOpen := mintListed(t, handler, accessCarol, "carol-open", nil)
	const list = "/api/v1/me/agent-tokens"
	keepScopes := map[string]any{"scopes": model.Scopes}
	readers := []struct {
		name string
		read func(method, path string, body any) *httptest.ResponseRecorder
	}{
		{"alice's browser", func(method, path string, body any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, method, path, body, "alice")
		}},
		{"carol's browser", func(method, path string, body any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, method, path, body, accessCarol)
		}},
		{"carol's token listing nothing", func(method, path string, body any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, method, path, body, carolOpen.Token)
		}},
		{"carol's token listing PRIV", func(method, path string, body any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, method, path, body, carolPriv.Token)
		}},
	}
	// Each reader's own token that a PATCH below edits, keeping its list.
	edited := map[string]string{
		"alice's browser":               alicePriv.ID,
		"carol's browser":               carolPriv.ID,
		"carol's token listing nothing": carolPriv.ID,
		"carol's token listing PRIV":    carolBoth.ID,
	}

	t.Run("with no private project every answer is the answer before the field", func(t *testing.T) {
		for _, reader := range readers {
			listed := reader.read(http.MethodGet, list, nil)
			if listed.Code != http.StatusOK {
				t.Fatalf("GET %s under %s: status=%d body=%s", list, reader.name, listed.Code, listed.Body.String())
			}
			wantAnswerBeforeHidden(t, "GET "+list+" under "+reader.name, listed.Body.String(), true)
			patched := reader.read(http.MethodPatch, list+"/"+edited[reader.name], keepScopes)
			if patched.Code != http.StatusOK {
				t.Fatalf("PATCH under %s: status=%d body=%s", reader.name, patched.Code, patched.Body.String())
			}
			wantAnswerBeforeHidden(t, "PATCH "+list+"/{id} under "+reader.name, patched.Body.String(), false)
		}
	})

	makePrivate(t, database, privateProject)

	t.Run("alice's browser is told which of her tokens list projects she cannot see", func(t *testing.T) {
		call := "GET " + list + " as alice"
		body := dispatchRequest(t, handler, http.MethodGet, list, nil, "alice").Body.String()
		for _, token := range []struct {
			name, id string
			projects []string
			hidden   bool
		}{
			{"alice-priv", alicePriv.ID, nil, true},
			{"alice-both", aliceBoth.ID, []string{publicProject}, true},
			{"alice-pub", alicePub.ID, []string{publicProject}, false},
			{"alice-open", aliceOpen.ID, nil, false},
		} {
			projects, _ := tokenListIn(t, call, httptestBody(body), token.id)
			hidden, present := hiddenFlagOf(t, call, body, token.id)
			if !slices.Equal(projects, token.projects) || hidden != token.hidden || present != token.hidden {
				t.Fatalf("%s gives %s projects %v and projects_hidden %v (present %v), want %v and %v (present only when true): %s",
					call, token.name, projects, hidden, present, token.projects, token.hidden, body)
			}
		}
		patch := "PATCH " + list + "/{id} as alice keeping alice-priv's list"
		patched := dispatchRequest(t, handler, http.MethodPatch, list+"/"+alicePriv.ID, keepScopes, "alice")
		if hidden, _ := hiddenFlagOf(t, patch, patched.Body.String(), alicePriv.ID); patched.Code != http.StatusOK || !hidden {
			t.Fatalf("%s = %d %s, want 200 with projects_hidden true", patch, patched.Code, patched.Body.String())
		}
	})

	t.Run("no credential is told a sibling token lists a project it cannot see", func(t *testing.T) {
		for _, reader := range readers[2:] {
			listed := reader.read(http.MethodGet, list, nil)
			patched := reader.read(http.MethodPatch, list+"/"+edited[reader.name], keepScopes)
			for call, answer := range map[string]*httptest.ResponseRecorder{
				"GET " + list + " under " + reader.name:        listed,
				"PATCH " + list + "/{id} under " + reader.name: patched,
			} {
				if answer.Code != http.StatusOK || strings.Contains(answer.Body.String(), "projects_hidden") {
					t.Fatalf("%s = %d %s, want 200 with no projects_hidden on any token", call, answer.Code, answer.Body.String())
				}
			}
		}
	})
}

// httptestBody is body as a 200 answer, for the helpers that read a recorder.
func httptestBody(body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	recorder.Code = http.StatusOK
	recorder.Body.WriteString(body)
	return recorder
}
