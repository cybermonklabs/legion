package api

import (
	"context"
	"crypto/sha256"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// heldToken is what a mint's, an edit's or a list's answer says a personal token holds.
type heldToken struct {
	ID       string   `json:"id"`
	Token    string   `json:"token"`
	Scopes   []string `json:"scopes"`
	Projects []string `json:"projects"`
}

// accessDana is a person from outside the sign-in domain who signs in first as a full member, so a
// row can make her a restricted member after her tokens were minted.
const accessDana = "dana@other.example"

// restrictedMintRig is the access server with outside sign-in on (newAccessServerWith): ACC, which
// alice owns, its list naming erin, a restricted member, and carol a full member; and OTH, a
// second project of alice's, whose list names nobody until a row puts erin on it.
type restrictedMintRig struct {
	handler  http.Handler
	database *store.Store
}

func newRestrictedMintRig(t *testing.T) restrictedMintRig {
	t.Helper()
	return newRestrictedMintRigWith(t, testServerOptions{})
}

// newRestrictedMintRigWith is newRestrictedMintRig's rig under options, which admit restricted
// members whatever else they set.
func newRestrictedMintRigWith(t *testing.T, options testServerOptions) restrictedMintRig {
	t.Helper()
	options.admitsRestricted = true
	handler, database := newAccessServerWith(t, options)
	if listed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Other project"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("alice creates OTH: status=%d body=%s", created.Code, created.Body.String())
	}
	return restrictedMintRig{handler: handler, database: database}
}

// mintAt is login's mint at their browser with body.
func (r restrictedMintRig) mintAt(t *testing.T, login string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/me/agent-tokens", body, login)
}

// minted is the token a 201 mint answered, failing the test for any other answer.
func minted(t *testing.T, response *httptest.ResponseRecorder, who string) heldToken {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s, want 201", who, response.Code, response.Body.String())
	}
	return decodeBody[heldToken](t, response)
}

// tokensOf counts owner's personal tokens.
func (r restrictedMintRig) tokensOf(t *testing.T, owner string) int64 {
	t.Helper()
	return countRows(t, r.database, `select count(*) from agent_tokens where canonical_login(owner) = $1`, owner)
}

// tokenIDOf is the id of the token whose plaintext is plaintext.
func (r restrictedMintRig) tokenIDOf(t *testing.T, plaintext string) string {
	t.Helper()
	hash := sha256.Sum256([]byte(plaintext))
	var id string
	if err := r.database.Pool.QueryRow(context.Background(), `select id::text from agent_tokens where token_hash = $1`, hash[:]).Scan(&id); err != nil {
		t.Fatalf("read the id of a token: %v", err)
	}
	return id
}

// rowOf is what the row of the token id holds, read from the table.
func (r restrictedMintRig) rowOf(t *testing.T, id string) heldToken {
	t.Helper()
	held := heldToken{ID: id}
	if err := r.database.Pool.QueryRow(context.Background(), `select scopes, projects from agent_tokens where id = $1`, id).Scan(&held.Scopes, &held.Projects); err != nil {
		t.Fatalf("read token %s: %v", id, err)
	}
	return held
}

// wantRestrictedRefusal fails the test unless response is 403 RESTRICTED_MEMBER saying saying.
func wantRestrictedRefusal(t *testing.T, response *httptest.ResponseRecorder, who, call, saying string) {
	t.Helper()
	wantRefusal(t, response, http.StatusForbidden, codeRestrictedMember, who, call)
	if !strings.Contains(response.Body.String(), saying) {
		t.Fatalf("%s: %s = %s, want the refusal to say %q", who, call, response.Body.String(), saying)
	}
}

// writesInACC is a write a token makes in ACC as its session: the act an outside person's agent
// is for, which a token holding no scope still makes.
func writesInACC(t *testing.T, handler http.Handler, token, session string) *httptest.ResponseRecorder {
	t.Helper()
	return agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ACC", "title": "Written by " + session,
		"actor": map[string]string{"kind": "session", "id": session},
	}, token)
}

// TestARestrictedMembersMintHoldsNoScopeAndNamesTheirOneProject (R16): erin, a restricted member
// whom ACC's list alone names, mints a token at her browser naming nothing, and it holds no scope
// and lists exactly ACC. Her agent acts in ACC with it, and settles nothing of hers: it reads no
// person-scope route, mints no token, looks up no machine login and approves nothing, each 403
// OAUTH_SCOPE_REQUIRED. An explicit empty set of scopes is that same token.
//
// Control: give a restricted member's mint the caller's own scopes again (tokenScopes answering
// callerScopes for them), and the first check fails naming [person approve]; and the full member's
// row (TestAFullMembersMintStillHoldsPersonAndApprove) shows the rig mints both where it should.
func TestARestrictedMembersMintHoldsNoScopeAndNamesTheirOneProject(t *testing.T) {
	r := newRestrictedMintRig(t)

	token := minted(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent"}), "erin's default mint")
	if len(token.Scopes) != 0 || token.Scopes == nil || !slices.Equal(token.Projects, []string{"ACC"}) {
		t.Fatalf("erin's default mint holds scopes %v and lists %v, want no scope ([]) and exactly [ACC]", token.Scopes, token.Projects)
	}
	if row := r.rowOf(t, token.ID); len(row.Scopes) != 0 || !slices.Equal(row.Projects, []string{"ACC"}) {
		t.Fatalf("erin's token's row holds %v listing %v, want no scope listing [ACC]", row.Scopes, row.Projects)
	}
	whoami := agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, token.Token)
	if whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"scopes":[]`) || !strings.Contains(whoami.Body.String(), `"project":"ACC"`) {
		t.Fatalf("whoami under erin's token = %d %s, want 200 holding no scope, held to ACC", whoami.Code, whoami.Body.String())
	}
	if wrote := writesInACC(t, r.handler, token.Token, "erin-agent"); wrote.Code != http.StatusCreated {
		t.Fatalf("erin's token writes an issue of ACC = %d %s, want 201", wrote.Code, wrote.Body.String())
	}
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/inbox"},
		{http.MethodPost, "/api/v1/me/agent-tokens"},
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup"},
		{http.MethodPost, "/api/v1/credential-requests/00000000-0000-0000-0000-000000000001/approve"},
	} {
		response := agentRequest(t, r.handler, call.method, call.path, map[string]any{"name": "minted by erin's agent", "code": "ABCD-EFGH"}, token.Token)
		wantRefusal(t, response, http.StatusForbidden, codeScopeRequired, "erin's token", call.method+" "+call.path)
	}

	explicit := minted(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's second agent", "scopes": []string{}}), "erin's mint naming no scope")
	if len(explicit.Scopes) != 0 || !slices.Equal(explicit.Projects, []string{"ACC"}) {
		t.Fatalf("erin's mint of scopes [] holds %v listing %v, want no scope listing [ACC]", explicit.Scopes, explicit.Projects)
	}
}

// accessRows is what a write of ACC's access list moves: the people and groups its list names
// (project_access), the outside people's links (outside_invitations) and the events.
type accessRows struct{ listed, links, events int64 }

func (r restrictedMintRig) accessRows(t *testing.T) accessRows {
	t.Helper()
	return accessRows{
		listed: countRows(t, r.database, `select count(*) from project_access where project_key = 'ACC'`),
		links:  countRows(t, r.database, `select count(*) from outside_invitations`),
		events: allEvents(t, r.database),
	}
}

// everyRow is databaseFingerprint with agent_tokens read without last_used_at, which a token's use
// writes at most once a minute whatever the call answers (personalTokenActor): two reads around a
// call match exactly where it wrote no row of any table.
func (r restrictedMintRig) everyRow(t *testing.T) map[string]string {
	t.Helper()
	fingerprint := databaseFingerprint(t, r.database)
	var tokens string
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) || ':' ||
		coalesce(md5(string_agg((to_jsonb(t) - 'last_used_at')::text, ',' order by t.id)), '') from agent_tokens t`).Scan(&tokens); err != nil {
		t.Fatalf("fingerprint agent_tokens: %v", err)
	}
	fingerprint["agent_tokens"] = tokens
	return fingerprint
}

// TestARestrictedMembersMintedTokenWritesNoAccessList (R16): erin, a restricted member whom ACC's
// list names, mints a token at her browser naming nothing, which holds no scope and lists exactly
// ACC, and with it writes nothing of ACC's access list: naming dave on it, taking carol off it and
// making her own link are each 403 OAUTH_SCOPE_REQUIRED, a scope check's refusal, and no row of
// any table changes. Two scope checks hold these routes, either alone enough: the table's gate
// (gateRouteScopes, routeScopes) and actingPerson's hold of person behind it. In the same run
// alice, ACC's owner and a full member, makes the same writes with her own token holding person
// and approve: dave is named and carol taken off, each 200 moving ACC's list and the events, and
// her token's link reaches its handler, 403 BROWSER_SESSION_REQUIRED (her token, minted naming no
// project, lists none, and only a personal token of the owner's that lists ACC makes its link),
// which her browser makes, 201 storing it. So each refusal of erin's is a scope check's, not the
// rig's.
//
// Controls: let every credential through both scope checks (gateRouteScopes never calling
// refuseMissingScope, and actingPerson never refusing a credential without person), and each of
// erin's writes reaches the owner's check and the row fails naming 403 PROJECT_OWNER_REQUIRED;
// with either check let through alone, the other refuses her and the row passes. Give a restricted
// member's mint the caller's scopes (tokenScopes not reading restricted), and the row fails at the
// mint naming [person approve], while each write is still refused, the scopes dropped at the
// token's use (personalTokenActor).
func TestARestrictedMembersMintedTokenWritesNoAccessList(t *testing.T) {
	r := newRestrictedMintRigWith(t, testServerOptions{passkeyIssuer: passkeyOutsideOrigin})
	if listed := dispatchRequest(t, r.handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names carol on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	writes := []struct{ method, path string }{
		{http.MethodPut, "/api/v1/projects/ACC/access/people/" + accessDave},
		{http.MethodDelete, "/api/v1/projects/ACC/access/people/" + accessCarol},
		{http.MethodPost, "/api/v1/projects/ACC/access/people/" + accessErin + "/invitations"},
	}

	token := minted(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent"}), "erin's default mint")
	if len(token.Scopes) != 0 || token.Scopes == nil || !slices.Equal(token.Projects, []string{"ACC"}) {
		t.Errorf("erin's default mint holds scopes %v and lists %v, want no scope ([]) and exactly [ACC]", token.Scopes, token.Projects)
	}
	for _, write := range writes {
		call := write.method + " " + write.path
		rows, tables := r.accessRows(t), r.everyRow(t)
		response := agentRequest(t, r.handler, write.method, write.path, nil, token.Token)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeScopeRequired+`"`) {
			t.Errorf("erin's token: %s = %d %s, want 403 %s", call, response.Code, response.Body.String(), codeScopeRequired)
		}
		if after := r.accessRows(t); after != rows {
			t.Errorf("erin's token: %s moved ACC's list, the links and the events from %+v to %+v, want them as they were", call, rows, after)
		}
		assertUnchanged(t, "erin's token's "+call, tables, r.everyRow(t))
	}

	owner := minted(t, r.mintAt(t, "alice", map[string]any{"name": "alice's agent"}), "alice's default mint")
	if !slices.Equal(owner.Scopes, model.Scopes) {
		t.Fatalf("alice's default mint holds %v, want %v", owner.Scopes, model.Scopes)
	}
	start := r.accessRows(t)
	for i, want := range []accessRows{
		{listed: start.listed + 1, links: start.links, events: start.events + 1},
		{listed: start.listed, links: start.links, events: start.events + 2},
	} {
		call := writes[i].method + " " + writes[i].path
		tables := r.everyRow(t)
		if response := agentRequest(t, r.handler, writes[i].method, writes[i].path, nil, owner.Token); response.Code != http.StatusOK {
			t.Errorf("alice's token: %s = %d %s, want 200", call, response.Code, response.Body.String())
		}
		if after := r.accessRows(t); after != want {
			t.Errorf("alice's token: %s left ACC's list, the links and the events at %+v, want %+v", call, after, want)
		}
		if maps.Equal(tables, r.everyRow(t)) {
			t.Errorf("alice's token: %s changed no table's fingerprint, so the fingerprint sees no write", call)
		}
	}
	link := writes[2]
	call := link.method + " " + link.path
	rows, tables := r.accessRows(t), r.everyRow(t)
	if response := agentRequest(t, r.handler, link.method, link.path, nil, owner.Token); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), `"code":"BROWSER_SESSION_REQUIRED"`) {
		t.Errorf("alice's token: %s = %d %s, want its handler's 403 BROWSER_SESSION_REQUIRED", call, response.Code, response.Body.String())
	}
	assertUnchanged(t, "alice's token's "+call, tables, r.everyRow(t))
	if made := dispatchRequest(t, r.handler, link.method, link.path, nil, "alice"); made.Code != http.StatusCreated {
		t.Errorf("alice at her browser: %s = %d %s, want 201", call, made.Code, made.Body.String())
	}
	if after := r.accessRows(t); after != (accessRows{listed: rows.listed, links: rows.links + 1, events: rows.events}) {
		t.Errorf("alice at her browser: %s left ACC's list, the links and the events at %+v from %+v, want one more link", call, after, rows)
	}
}

// TestARestrictedMemberIsRefusedAScopeOrAListOfOtherThanOneProject (R16): a restricted member's
// mint or edit that asks for a scope is 403 RESTRICTED_MEMBER saying the token holds none, and one
// whose list would name other than one project is 403 RESTRICTED_MEMBER saying it lists exactly
// one, each writing nothing. Seeing two projects, erin's mint naming none is refused and asks her
// to name one; naming one she sees mints it, and an edit moves it to the other. A token of hers
// listing none, which a mint before the rule made, is refused an edit that keeps its list, and is
// held once an edit names one project. Taken off OTH's list, her edit that keeps the list of a
// token naming OTH alone, stored holding person and approve, is 403 RESTRICTED_MEMBER saying its
// project is no project she sees, and the token's row is as it was; the same edit of her token of
// ACC, which she still sees, is made.
//
// Controls: refuse nothing for a restricted member (tokenScopes and tokenProjects as before), and
// the first refusal fails, erin's mint of person answering 201. Refuse no kept list naming a
// project she does not see (restrictedKeptListRefusal's second case answering none), and the edit
// of her OTH token, once she is off OTH's list, answers 200 and the row fails there; with
// restrictedKeptListRefusal answering no refusal at all, the row fails earlier, the edit keeping a
// list of no project answering 200.
func TestARestrictedMemberIsRefusedAScopeOrAListOfOtherThanOneProject(t *testing.T) {
	r := newRestrictedMintRig(t)
	before := r.tokensOf(t, accessErin)
	for _, scopes := range [][]string{{model.ScopePerson}, {model.ScopePerson, model.ScopeApprove}} {
		wantRestrictedRefusal(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent", "scopes": scopes}),
			"erin at her browser", "a mint asking for "+strings.Join(scopes, " and "), "holds no scope")
	}
	wantRestrictedRefusal(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent", "projects": []string{}}),
		"erin at her browser", "a mint listing no project", "exactly one project")
	if after := r.tokensOf(t, accessErin); after != before {
		t.Fatalf("erin's refused mints moved her tokens from %d to %d", before, after)
	}

	if listed := dispatchRequest(t, r.handler, http.MethodPut, "/api/v1/projects/OTH/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on OTH's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	wantRestrictedRefusal(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent"}),
		"erin at her browser, seeing ACC and OTH", "a mint naming no project", "name the one")
	wantRestrictedRefusal(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's agent", "projects": []string{"ACC", "OTH"}}),
		"erin at her browser", "a mint listing ACC and OTH", "exactly one project")
	if after := r.tokensOf(t, accessErin); after != before {
		t.Fatalf("erin's refused mints moved her tokens from %d to %d", before, after)
	}
	token := minted(t, r.mintAt(t, accessErin, map[string]any{"name": "erin's OTH agent", "projects": []string{"OTH"}, "scopes": []string{}}), "erin's mint naming OTH")
	if len(token.Scopes) != 0 || !slices.Equal(token.Projects, []string{"OTH"}) {
		t.Fatalf("erin's mint naming OTH holds %v listing %v, want no scope listing [OTH]", token.Scopes, token.Projects)
	}

	edit := func(id string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, r.handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+id, body, accessErin)
	}
	wantRestrictedRefusal(t, edit(token.ID, map[string]any{"scopes": []string{model.ScopePerson}}),
		"erin at her browser", "an edit asking for person", "holds no scope")
	wantRestrictedRefusal(t, edit(token.ID, map[string]any{"scopes": []string{}, "projects": []string{"ACC", "OTH"}}),
		"erin at her browser", "an edit listing ACC and OTH", "exactly one project")
	if row := r.rowOf(t, token.ID); len(row.Scopes) != 0 || !slices.Equal(row.Projects, []string{"OTH"}) {
		t.Fatalf("erin's OTH token after the refused edits holds %v listing %v, want no scope listing [OTH]", row.Scopes, row.Projects)
	}
	moved := edit(token.ID, map[string]any{"scopes": []string{}, "projects": []string{"ACC"}})
	if moved.Code != http.StatusOK {
		t.Fatalf("erin moves her token to ACC: status=%d body=%s, want 200", moved.Code, moved.Body.String())
	}
	if got := decodeBody[heldToken](t, moved); len(got.Scopes) != 0 || !slices.Equal(got.Projects, []string{"ACC"}) {
		t.Fatalf("erin's moved token holds %v listing %v, want no scope listing [ACC]", got.Scopes, got.Projects)
	}

	unlisted := personalTokenListing(t, r.database, accessErin, nil)
	unlistedID := r.tokenIDOf(t, unlisted)
	wantRestrictedRefusal(t, edit(unlistedID, map[string]any{"scopes": []string{}}),
		"erin at her browser", "an edit keeping a list of no project", "exactly one project")
	if kept := edit(unlistedID, map[string]any{"scopes": []string{}, "projects": []string{"OTH"}}); kept.Code != http.StatusOK {
		t.Fatalf("erin names OTH on her token that listed none: status=%d body=%s, want 200", kept.Code, kept.Body.String())
	}
	if whoami := agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, unlisted); whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"project":"OTH"`) {
		t.Fatalf("whoami under erin's token once it names OTH = %d %s, want 200 held to OTH", whoami.Code, whoami.Body.String())
	}

	stale := personalTokenListing(t, r.database, accessErin, []string{"OTH"}, model.ScopePerson, model.ScopeApprove)
	staleID := r.tokenIDOf(t, stale)
	if removed := dispatchRequest(t, r.handler, http.MethodDelete, "/api/v1/projects/OTH/access/people/"+accessErin, nil, "alice"); removed.Code != http.StatusOK {
		t.Fatalf("alice takes erin off OTH's list: status=%d body=%s", removed.Code, removed.Body.String())
	}
	wantRestrictedRefusal(t, edit(staleID, map[string]any{"scopes": []string{}}),
		"erin at her browser, taken off OTH's list", "an edit keeping a list of OTH", "no project this request's credential sees")
	if row := r.rowOf(t, staleID); !slices.Equal(row.Scopes, []string{model.ScopePerson, model.ScopeApprove}) || !slices.Equal(row.Projects, []string{"OTH"}) {
		t.Fatalf("erin's OTH token after the refused edit holds %v listing %v, want it as stored, [person approve] listing [OTH]", row.Scopes, row.Projects)
	}
	kept := edit(token.ID, map[string]any{"scopes": []string{}})
	if kept.Code != http.StatusOK {
		t.Fatalf("erin's edit keeping the list of her ACC token, taken off OTH's list: status=%d body=%s, want 200", kept.Code, kept.Body.String())
	}
	if got := decodeBody[heldToken](t, kept); len(got.Scopes) != 0 || !slices.Equal(got.Projects, []string{"ACC"}) {
		t.Fatalf("erin's edited ACC token holds %v listing %v, want no scope listing [ACC]", got.Scopes, got.Projects)
	}
}

// TestAStoredRestrictedTokenHoldingApproveActsWithNoScope (R16, R3): a token of erin's that a
// mint before the rule gave person and approve, listing ACC, acts with no scope: whoami answers
// none, and it mints no token, edits or revokes none, revokes no grant or sign-in, and dismisses
// or decides no credential request, each 403 OAUTH_SCOPE_REQUIRED, and nothing is written; it
// still writes in ACC, so its scopes were dropped, not the token. Her browser's list says the
// token holds none. A token of hers listing no project acts for nobody: 403 RESTRICTED_MEMBER
// saying a restricted member's token lists exactly one.
//
// Control: drop nothing at the token's use (personalTokenActor answering the row's scopes), and
// the first check fails on whoami's scopes, whoami answering person and approve, and the test
// stops there; at 809a2c288d the grant and sign-in revokes reached their handler (404 FEATURE_OFF
// here, where no broker is configured).
func TestAStoredRestrictedTokenHoldingApproveActsWithNoScope(t *testing.T) {
	r := newRestrictedMintRig(t)
	stored := personalTokenListing(t, r.database, accessErin, []string{"ACC"}, model.ScopePerson, model.ScopeApprove)
	id := r.tokenIDOf(t, stored)
	before, events := r.tokensOf(t, accessErin), allEvents(t, r.database)

	whoami := agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, stored)
	if whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"scopes":[]`) {
		t.Fatalf("whoami under erin's stored token = %d %s, want 200 holding no scope", whoami.Code, whoami.Body.String())
	}
	const someID = "00000000-0000-0000-0000-000000000001"
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted by erin's stored token"}},
		{http.MethodPatch, "/api/v1/me/agent-tokens/" + id, map[string]any{"scopes": []string{model.ScopePerson, model.ScopeApprove}}},
		{http.MethodDelete, "/api/v1/me/agent-tokens/" + id, nil},
		{http.MethodPost, "/api/v1/credential-grants/" + someID + "/revoke", nil},
		{http.MethodPost, "/api/v1/signins/" + someID + "/revoke", nil},
		{http.MethodPost, "/api/v1/credential-requests/" + someID + "/dismiss", nil},
		{http.MethodPost, "/api/v1/credential-requests/" + someID + "/approve", map[string]any{"code": "ABCD-EFGH"}},
		{http.MethodPost, "/api/v1/machine-logins/" + someID + "/revoke", nil},
	} {
		response := agentRequest(t, r.handler, call.method, call.path, call.body, stored)
		wantRefusal(t, response, http.StatusForbidden, codeScopeRequired, "erin's stored token", call.method+" "+call.path)
	}
	if after := r.tokensOf(t, accessErin); after != before {
		t.Fatalf("erin's stored token's refused calls moved her tokens from %d to %d", before, after)
	}
	if row := r.rowOf(t, id); !slices.Equal(row.Scopes, []string{model.ScopePerson, model.ScopeApprove}) {
		t.Fatalf("erin's stored token's row holds %v after the refused edit, want it as stored", row.Scopes)
	}
	if after := allEvents(t, r.database); after != events {
		t.Fatalf("erin's stored token's refused calls moved events from %d to %d", events, after)
	}
	if wrote := writesInACC(t, r.handler, stored, "erin-agent"); wrote.Code != http.StatusCreated {
		t.Fatalf("erin's stored token writes an issue of ACC = %d %s, want 201: its scopes are dropped, not the token", wrote.Code, wrote.Body.String())
	}

	listed := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, accessErin)
	if listed.Code != http.StatusOK {
		t.Fatalf("erin lists her tokens: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, token := range decodeBody[[]heldToken](t, listed) {
		if token.ID == id && (len(token.Scopes) != 0 || token.Scopes == nil) {
			t.Fatalf("erin's browser lists her stored token holding %v, want [], the scopes it acts with", token.Scopes)
		}
	}

	unlisted := personalTokenListing(t, r.database, accessErin, nil, model.ScopePerson, model.ScopeApprove)
	wantRestrictedRefusal(t, agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, unlisted),
		"erin's stored token listing no project", "GET /api/v1/whoami", "exactly one project")
}

// TestAFullMembersMintStillHoldsPersonAndApprove: carol, a full member, mints as she did before the
// rule for restricted members: at her browser naming nothing, a token holding person and approve
// and listing none; that token mints another holding both, and an edit narrows it to person and
// widens it back.
//
// Control: give every caller's mint the restricted member's scopes (tokenScopes reading every
// caller as restricted, the mint handing it true), and the first check fails, carol's default mint
// holding [] where the row wants [person approve].
func TestAFullMembersMintStillHoldsPersonAndApprove(t *testing.T) {
	r := newRestrictedMintRig(t)
	token := minted(t, r.mintAt(t, accessCarol, map[string]any{"name": "carol's agent"}), "carol's default mint")
	if !slices.Equal(token.Scopes, model.Scopes) || len(token.Projects) != 0 {
		t.Fatalf("carol's default mint holds %v listing %v, want %v listing none", token.Scopes, token.Projects, model.Scopes)
	}
	again := agentRequest(t, r.handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted by carol's agent"}, token.Token)
	if child := minted(t, again, "carol's token's mint"); !slices.Equal(child.Scopes, model.Scopes) {
		t.Fatalf("carol's token mints a token holding %v, want %v", child.Scopes, model.Scopes)
	}
	for _, scopes := range [][]string{{model.ScopePerson}, model.Scopes} {
		edited := dispatchRequest(t, r.handler, http.MethodPatch, "/api/v1/me/agent-tokens/"+token.ID, map[string]any{"scopes": scopes}, accessCarol)
		if edited.Code != http.StatusOK || !slices.Equal(decodeBody[heldToken](t, edited).Scopes, scopes) {
			t.Fatalf("carol edits her token to %v: status=%d body=%s, want 200 holding them", scopes, edited.Code, edited.Body.String())
		}
	}
}

// TestAMemberMadeRestrictedActsWithNoScopeThroughTheirTokens (R16): dana minted two tokens as a
// full member, one listing ACC and one listing none, each holding person and approve. Once ACC's
// list names her and she is a restricted member, the token listing ACC acts with no scope - it
// mints no token and revokes none, 403 OAUTH_SCOPE_REQUIRED, and still writes in ACC - and the
// token listing none acts for nobody, 403 RESTRICTED_MEMBER. Her browser's list says each holds
// none. Before the change of kind, the same token minted a token, so each refusal is the change's.
//
// Control: drop nothing at a token's use (personalTokenActor answering the row's scopes), and the
// row fails at its whoami check once she is restricted, whoami under her ACC token answering
// person and approve where the row wants none, and stops there.
func TestAMemberMadeRestrictedActsWithNoScopeThroughTheirTokens(t *testing.T) {
	r := newRestrictedMintRig(t)
	seedPeople(t, r.database, accessDana)
	listing := minted(t, r.mintAt(t, accessDana, map[string]any{"name": "dana's ACC agent", "projects": []string{"ACC"}}), "dana's mint listing ACC, as a full member")
	unlisted := minted(t, r.mintAt(t, accessDana, map[string]any{"name": "dana's agent"}), "dana's default mint, as a full member")
	for _, token := range []heldToken{listing, unlisted} {
		if !slices.Equal(token.Scopes, model.Scopes) {
			t.Fatalf("dana's mint as a full member holds %v, want %v", token.Scopes, model.Scopes)
		}
	}
	minted(t, agentRequest(t, r.handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted while full"}, listing.Token), "dana's token's mint while she is a full member")

	if listed := dispatchRequest(t, r.handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessDana, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names dana on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	markRestricted(t, r.database, accessDana)

	before := r.tokensOf(t, accessDana)
	whoami := agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, listing.Token)
	if whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), `"scopes":[]`) || !strings.Contains(whoami.Body.String(), `"member_kind":"restricted"`) {
		t.Fatalf("whoami under dana's ACC token once she is restricted = %d %s, want 200, a restricted member's, holding no scope", whoami.Code, whoami.Body.String())
	}
	wantRefusal(t, agentRequest(t, r.handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "minted once restricted"}, listing.Token),
		http.StatusForbidden, codeScopeRequired, "dana's ACC token once she is restricted", "POST /api/v1/me/agent-tokens")
	wantRefusal(t, agentRequest(t, r.handler, http.MethodDelete, "/api/v1/me/agent-tokens/"+unlisted.ID, nil, listing.Token),
		http.StatusForbidden, codeScopeRequired, "dana's ACC token once she is restricted", "DELETE /api/v1/me/agent-tokens/{id}")
	if after := r.tokensOf(t, accessDana); after != before {
		t.Fatalf("dana's refused mint moved her tokens from %d to %d", before, after)
	}
	if wrote := writesInACC(t, r.handler, listing.Token, "dana-agent"); wrote.Code != http.StatusCreated {
		t.Fatalf("dana's ACC token writes an issue of ACC once she is restricted = %d %s, want 201", wrote.Code, wrote.Body.String())
	}
	wantRestrictedRefusal(t, agentRequest(t, r.handler, http.MethodGet, "/api/v1/whoami", nil, unlisted.Token),
		"dana's token listing no project once she is restricted", "GET /api/v1/whoami", "exactly one project")

	listed := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/me/agent-tokens", nil, accessDana)
	if listed.Code != http.StatusOK {
		t.Fatalf("dana lists her tokens: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, token := range decodeBody[[]heldToken](t, listed) {
		if len(token.Scopes) != 0 {
			t.Fatalf("dana's browser lists token %s holding %v once she is restricted, want []", token.ID, token.Scopes)
		}
	}
}

// TestTheOwnLoginRefusalHoldsARestrictedCredentialHoldingTheScopes: no credential of a restricted
// member holds a scope any more (personalTokenActor, brokerTokenActor), so the scope gate refuses
// each of their tokens on the rows that decide, list or end a machine login before the gates behind
// it run. Those gates still hold such a credential on their own: a credential of erin's holding
// person and approve, presented as no browser, reaches withinProjectScope on the lookup, the
// approve, the deny and the revoke, and withinReadScope on the list, and each answers 403
// RESTRICTED_MEMBER naming the browser before its handler runs.
//
// Control: let any credential of a restricted member through (restrictedOwnLoginAtBrowser answering
// allowed true), and each handler runs and the row fails naming its route.
func TestTheOwnLoginRefusalHoldsARestrictedCredentialHoldingTheScopes(t *testing.T) {
	handler, _, served := newRegisteredAccessServer(t, testServerOptions{admitsRestricted: true})
	if listed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	owner := accessErin
	holding := model.Actor{Kind: "session", Owner: &owner,
		Credential: &model.Credential{Scopes: model.Scopes, TokenID: "a-credential-holding-the-scopes", Projects: []string{"ACC"}}}
	table := served.routes()
	for _, row := range []struct{ method, pattern, path string }{
		{http.MethodPost, "/api/v1/credential-requests/machine-lookup", "/api/v1/credential-requests/machine-lookup"},
		{http.MethodPost, "/api/v1/credential-requests/{id}/approve", "/api/v1/credential-requests/rec-erin/approve"},
		{http.MethodPost, "/api/v1/credential-requests/{id}/deny", "/api/v1/credential-requests/rec-erin/deny"},
		{http.MethodPost, "/api/v1/machine-logins/{id}/revoke", "/api/v1/machine-logins/cred-erin/revoke"},
		{http.MethodGet, "/api/v1/machine-logins", "/api/v1/machine-logins"},
	} {
		at := slices.IndexFunc(table, func(route apiRoute) bool { return route.Method == row.method && route.Pattern == row.pattern })
		if at < 0 {
			t.Fatalf("the table holds no row %s %s", row.method, row.pattern)
		}
		ran := false
		next := func(w http.ResponseWriter, _ *http.Request) {
			ran = true
			w.WriteHeader(http.StatusNoContent)
		}
		gate := served.withinProjectScope(table[at], next)
		if row.method == http.MethodGet {
			gate = served.withinReadScope(table[at], next)
		}
		request := httptest.NewRequest(row.method, row.path, strings.NewReader(`{"code":"ABCD-1234"}`))
		request = request.WithContext(context.WithValue(request.Context(), resolvedCallerKey{}, resolvedCaller{actor: holding}))
		response := httptest.NewRecorder()
		gate(response, request)
		if ran || response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"`+codeRestrictedMember+`"`) ||
			!strings.Contains(response.Body.String(), "never through a token") {
			t.Errorf("%s %s with a credential of erin's holding %v: handler ran %t, answered %d %s; want 403 %s naming the browser and no handler",
				row.method, row.pattern, model.Scopes, ran, response.Code, response.Body.String(), codeRestrictedMember)
		}
	}
}

// TestARestrictedMembersBrokerTokenListingOtherThanOneProjectActsForNobody (R16: an outside
// person's agent's token lists exactly one project): the broker mints a restricted member's machine
// login the named scope with the one project its trade names, but a broker whose project check is
// off, an older one, or a token minted before that rule can answer a token of theirs listing none
// or several. Dispatch holds it as it holds their personal token: erin's person-scope token listing
// none, and one listing ACC and OTH, are each 403 RESTRICTED_MEMBER saying the token lists exactly
// one project, on a request and at a socket's heartbeat (callerStillResolves). Her token listing
// ACC alone acts, holding no scope, in either project scope. carol's person-scope token listing
// none, a full member's, acts with the person scope as before, so each refusal is the member's.
//
// Control: restrictedBrokerTokenList answering nil, and erin's token listing none answers 200 and
// the row fails at its first check.
func TestARestrictedMembersBrokerTokenListingOtherThanOneProjectActsForNobody(t *testing.T) {
	stub := ciphertokentest.NewStubBroker(t)
	handler, _, served := newRegisteredAccessServer(t, testServerOptions{admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub)})
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTH", "name": "Other project"}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("alice creates OTH: status=%d body=%s", created.Code, created.Body.String())
	}
	for _, project := range []string{"ACC", "OTH"} {
		if listed := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
			t.Fatalf("alice names erin on %s's list: status=%d body=%s", project, listed.Code, listed.Body.String())
		}
	}
	caller := newBrokerCaller(t)
	says := func(token, person, scope string, projects ...string) {
		answer := liveNamedToken(caller.jkt)
		answer.Person, answer.ProjectScope, answer.Projects = person, scope, projects
		stub.Says(token, answer)
	}
	whoami := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		return response
	}
	for _, refused := range []struct {
		token    string
		projects []string
	}{
		{"erins-token-listing-none", nil},
		{"erins-token-listing-two", []string{"ACC", "OTH"}},
	} {
		says(refused.token, accessErin, ciphertoken.ProjectScopePerson, refused.projects...)
		wantRestrictedRefusal(t, whoami(refused.token), "erin's broker token listing "+projectCount(len(refused.projects)), "GET /api/v1/whoami", "exactly one project")
		_, _, err := served.callerStillResolves(caller.presenting(t, http.MethodGet, "/ws/live", refused.token, nil))
		wantAPIError(t, err, http.StatusForbidden, codeRestrictedMember, "the heartbeat of a socket erin's broker token listing "+projectCount(len(refused.projects))+" opened")
	}
	for _, acts := range []struct{ token, scope string }{
		{"erins-token-listing-acc", ciphertoken.ProjectScopePerson},
		{"erins-named-token-of-acc", ciphertoken.ProjectScopeNamed},
	} {
		says(acts.token, accessErin, acts.scope, "ACC")
		if response := whoami(acts.token); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"scopes":[]`) {
			t.Errorf("erin's %s broker token listing ACC: GET /api/v1/whoami = %d %s, want 200 holding no scope", acts.scope, response.Code, response.Body.String())
		}
	}
	says("carols-token-listing-none", accessCarol, ciphertoken.ProjectScopePerson)
	if response := whoami("carols-token-listing-none"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"scopes":["person"]`) {
		t.Errorf("carol's person-scope broker token listing none: GET /api/v1/whoami = %d %s, want 200 holding the person scope", response.Code, response.Body.String())
	}
}
