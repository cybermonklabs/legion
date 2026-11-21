package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The people the access tests name beside the server's own alice and bob: carol and dave are full
// members, and erin a restricted member.
const (
	accessCarol = "carol@example.com"
	accessDave  = "dave@example.com"
	accessErin  = "erin@other.example"
)

// newAccessServer is a test server holding project ACC, which alice created and so owns, and the
// three access people.
func newAccessServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessCarol, accessDave, accessErin)
	markRestricted(t, database, accessErin)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "ACC", "name": "Access project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create ACC: status=%d body=%s", response.Code, response.Body.String())
	}
	return handler, database
}

func markRestricted(t *testing.T, database *store.Store, login string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update people set member_kind = 'restricted' where email = $1`, login); err != nil {
		t.Fatalf("mark %s restricted: %v", login, err)
	}
}

func readAccess(t *testing.T, handler http.Handler, login string) model.ProjectAccess {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/ACC/access", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("GET ACC's access as %s: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[model.ProjectAccess](t, response)
}

func allEvents(t *testing.T, database *store.Store) int64 {
	t.Helper()
	return countRows(t, database, `select count(*) from events`)
}

// wantRefusal fails the test unless response is status with code, naming who made the call.
func wantRefusal(t *testing.T, response *httptest.ResponseRecorder, status int, code, who, call string) {
	t.Helper()
	if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("%s: %s = %d %s, want %d %s", who, call, response.Code, response.Body.String(), status, code)
	}
}

// TestOnlyTheOwnerChangesTheAccessList: the owner names people and groups on the list and takes
// them off; anyone else is 403 PROJECT_OWNER_REQUIRED and nothing is written; a project cannot be
// made private where the deployment does not let one be.
func TestOnlyTheOwnerChangesTheAccessList(t *testing.T) {
	handler, database := newAccessServer(t)
	if got := readAccess(t, handler, "bob"); got.Owner != "alice" || got.Visibility != model.ProjectPublic || len(got.People) != 0 || got.People == nil || got.Groups == nil {
		t.Fatalf("ACC's access = %+v, want alice's public project with two empty lists", got)
	}

	for _, call := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/projects/ACC/access/people/" + accessCarol},
		{http.MethodDelete, "/api/v1/projects/ACC/access/people/" + accessCarol},
		{http.MethodPut, "/api/v1/projects/ACC/access/groups/crew"},
		{http.MethodDelete, "/api/v1/projects/ACC/access/groups/crew"},
		{http.MethodPatch, "/api/v1/projects/ACC/access"},
	} {
		before := allEvents(t, database)
		var body any
		if call.method == http.MethodPatch {
			body = map[string]string{"owner": "bob"}
		}
		wantRefusal(t, dispatchRequest(t, handler, call.method, call.path, body, "bob"), http.StatusForbidden,
			"PROJECT_OWNER_REQUIRED", "bob", call.method+" "+call.path)
		if after := allEvents(t, database); after != before {
			t.Fatalf("bob's refused %s %s moved events from %d to %d", call.method, call.path, before, after)
		}
	}

	added := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/Carol@Example.com", nil, "alice")
	if added.Code != http.StatusOK || !slices.Equal(decodeBody[model.ProjectAccess](t, added).People, []string{accessCarol}) {
		t.Fatalf("alice names carol: %d %s, want carol on the list", added.Code, added.Body.String())
	}
	if got := countRows(t, database, `select count(*) from events where type = 'project.access_changed' and project_key = 'ACC'`); got != 1 {
		t.Fatalf("project.access_changed events on ACC = %d, want 1", got)
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/groups/crew", nil, "alice"),
		http.StatusNotFound, "GROUP_NOT_FOUND", "alice", "PUT a group no row holds")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/not-an-email", nil, "alice"),
		http.StatusBadRequest, "PROJECT_ACCESS_INPUT", "alice", "PUT a person who is no email")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{"visibility": "private"}, "alice"),
		http.StatusConflict, "PRIVATE_PROJECTS_OFF", "alice", "PATCH visibility private")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{}, "alice"),
		http.StatusBadRequest, "PROJECT_ACCESS_INPUT", "alice", "PATCH naming nothing")
	wantRefusal(t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/NOPE/access", nil, "alice"),
		http.StatusNotFound, "PROJECT_NOT_FOUND", "alice", "GET an unknown project's access")

	removed := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, "alice")
	if removed.Code != http.StatusOK || len(decodeBody[model.ProjectAccess](t, removed).People) != 0 {
		t.Fatalf("alice takes carol off: %d %s, want an empty list", removed.Code, removed.Body.String())
	}
	// The shared token holds no person scope, so it reads no project's access at all.
	wantRefusal(t, agentRequest(t, handler, http.MethodGet, "/api/v1/projects/ACC/access", nil, "agent-token"),
		http.StatusForbidden, "OAUTH_SCOPE_REQUIRED", "the shared token", "GET ACC's access")
}

// TestAHiddenProjectsAccessListAnswersAsAnUnknownKey: GET /api/v1/projects/{key}/access answers a
// project the caller does not see exactly as a key no project holds, the same status and the same
// bytes, for alice, alice's token that lists PRIV and carol's token that lists none; bob and carol,
// who see PRIV, read its list. The control is the read made without the caller's viewer: alice then
// reads PRIV's list, and the test fails naming her.
func TestAHiddenProjectsAccessListAnswersAsAnUnknownKey(t *testing.T) {
	seed := seedPrivateProject(t)
	for _, caller := range []privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken} {
		hidden := seed.request(t, caller, http.MethodGet, "/api/v1/projects/"+seedPrivate+"/access", nil)
		missing := seed.request(t, caller, http.MethodGet, "/api/v1/projects/NOPE/access", nil)
		if want := strings.ReplaceAll(missing.Body.String(), "NOPE", seedPrivate); hidden.Code != missing.Code ||
			hidden.Body.String() != want || missing.Code != http.StatusNotFound {
			t.Errorf("%s: GET PRIV's access = %d %s, and a key no project holds %d %s, want both 404 PROJECT_NOT_FOUND alike",
				caller, hidden.Code, strings.TrimSpace(hidden.Body.String()), missing.Code, strings.TrimSpace(missing.Body.String()))
		}
	}
	for _, caller := range []privateCaller{callerBob, callerCarol} {
		read := seed.request(t, caller, http.MethodGet, "/api/v1/projects/"+seedPrivate+"/access", nil)
		if read.Code != http.StatusOK || decodeBody[model.ProjectAccess](t, read).Owner != seedCarol {
			t.Errorf("%s: GET PRIV's access = %d %s, want 200 and carol its owner", caller, read.Code, read.Body.String())
		}
	}
}

// TestTheOwnerGivesTheProjectToAFullMember: the owner gives the project to another full member at
// any time, and from then on only that person changes its access; a restricted member or a login
// nobody has signed in with cannot be given it.
func TestTheOwnerGivesTheProjectToAFullMember(t *testing.T) {
	handler, _ := newAccessServer(t)
	for _, refused := range []string{accessErin, "nobody@example.com"} {
		wantRefusal(t, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{"owner": refused}, "alice"),
			http.StatusBadRequest, "OWNER_NOT_ALLOWED", "alice", "give ACC to "+refused)
	}
	given := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{"owner": " Dave@Example.com "}, "alice")
	if given.Code != http.StatusOK || decodeBody[model.ProjectAccess](t, given).Owner != accessDave {
		t.Fatalf("alice gives ACC to dave: %d %s, want dave the owner", given.Code, given.Body.String())
	}
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, "alice"),
		http.StatusForbidden, "PROJECT_OWNER_REQUIRED", "alice", "PUT carol after giving ACC away")
	if added := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessCarol, nil, accessDave); added.Code != http.StatusOK {
		t.Fatalf("dave names carol on his project: %d %s", added.Code, added.Body.String())
	}
}

// TestTheAccessListWritesNeedTheApproveScope: naming a person or a group on a project's access
// list, taking one off, setting its visibility and giving it to another owner decide who may see
// the project, so a credential of the owner's that holds `person` alone - a personal token minted
// with that scope only, as personOnly is - is 403 OAUTH_SCOPE_REQUIRED naming `approve` on each,
// and nothing is written. The owner's credential that holds `approve` as well makes each change,
// and reading the access list still needs `person` alone.
func TestTheAccessListWritesNeedTheApproveScope(t *testing.T) {
	handler, database := newAccessServer(t)
	if made := dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/crew", map[string]any{"members": []string{accessCarol}}, "alice"); made.Code != http.StatusOK {
		t.Fatalf("alice makes crew: %d %s", made.Code, made.Body.String())
	}
	personOnly := personalTokenOwnedBy(t, database, "alice", model.ScopePerson)
	approving := personalTokenOwnedBy(t, database, "alice", model.ScopePerson, model.ScopeApprove)
	if read := agentRequest(t, handler, http.MethodGet, "/api/v1/projects/ACC/access", nil, personOnly); read.Code != http.StatusOK {
		t.Fatalf("alice's person token reads ACC's access: %d %s, want 200", read.Code, read.Body.String())
	}

	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/api/v1/projects/ACC/access/people/" + accessCarol, nil},
		{http.MethodPut, "/api/v1/projects/ACC/access/groups/crew", nil},
		{http.MethodDelete, "/api/v1/projects/ACC/access/groups/crew", nil},
		{http.MethodDelete, "/api/v1/projects/ACC/access/people/" + accessCarol, nil},
		{http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{"visibility": string(model.ProjectPublic)}},
		{http.MethodPatch, "/api/v1/projects/ACC/access", map[string]string{"owner": accessDave}},
	}
	for _, write := range writes {
		call := write.method + " " + write.path
		before, access := allEvents(t, database), readAccess(t, handler, "alice")
		wantApproveRefusal(t, agentRequest(t, handler, write.method, write.path, write.body, personOnly), call)
		if after, now := allEvents(t, database), readAccess(t, handler, "alice"); after != before || !slices.Equal(now.People, access.People) ||
			!slices.Equal(now.Groups, access.Groups) || now.Owner != access.Owner || now.Visibility != access.Visibility {
			t.Fatalf("the refused %s moved events from %d to %d and the access from %+v to %+v", call, before, after, access, now)
		}

		if admitted := agentRequest(t, handler, write.method, write.path, write.body, approving); admitted.Code != http.StatusOK {
			t.Fatalf("%s with alice's approving token: %d %s, want 200", call, admitted.Code, admitted.Body.String())
		}
	}
	if got := readAccess(t, handler, "alice"); got.Owner != accessDave || len(got.People) != 0 || len(got.Groups) != 0 {
		t.Fatalf("after the approving token's changes ACC's access = %+v, want dave's project with empty lists", got)
	}
}

// wantApproveRefusal fails the test unless response is the scope gate's refusal of a credential
// holding `person` alone on a row that needs `approve`: 403 OAUTH_SCOPE_REQUIRED naming approve,
// under RFC 6750's insufficient_scope challenge.
func wantApproveRefusal(t *testing.T, response *httptest.ResponseRecorder, call string) {
	t.Helper()
	if response.Code != http.StatusForbidden {
		t.Fatalf("%s with alice's person token: %d %s, want 403", call, response.Code, response.Body.String())
	}
	if body := decodeBody[refusalAnswer](t, response); body.Code != codeScopeRequired || !strings.Contains(body.Error, model.ScopeApprove) {
		t.Fatalf("%s with alice's person token: the refusal = %+v, want %s naming the approve scope", call, body, codeScopeRequired)
	}
	if got, want := response.Header().Get("WWW-Authenticate"), insufficientScopeChallenge(model.ScopeApprove); got != want {
		t.Fatalf("%s with alice's person token: the challenge = %q, want %q", call, got, want)
	}
}

// TestARestrictedMemberCannotCreateAProject: a restricted member's create is 403 RESTRICTED_MEMBER
// before anything is written, the refusal of a deployment write (withinProjectScope). A full
// member's create answers as it always did, and the creator owns the project; a create by a
// credential that acts for nobody gives the project its default assignee as owner.
func TestARestrictedMemberCannotCreateAProject(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessErin, accessCarol)
	markRestricted(t, database, accessErin)
	before := allEvents(t, database)
	wantRefusal(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OUT", "name": "Outside"}, accessErin),
		http.StatusForbidden, "RESTRICTED_MEMBER", accessErin, "POST /api/v1/projects")
	if got := countRows(t, database, `select count(*) from projects where key = 'OUT'`); got != 0 || allEvents(t, database) != before {
		t.Fatalf("a refused create stored %d projects and moved events from %d to %d", got, before, allEvents(t, database))
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "MINE", "name": "Mine"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("alice's create: %d %s", created.Code, created.Body.String())
	}
	var keys map[string]any
	keys = decodeBody[map[string]any](t, created)
	for _, key := range []string{"key", "name", "gated_steps", "default_assignee", "created_at"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("the create's answer %s lacks %s", created.Body.String(), key)
		}
	}
	if _, ok := keys["owner"]; ok || len(keys) > 6 {
		t.Errorf("the create's answer %s changed shape", created.Body.String())
	}
	if owner := projectOwnerOf(t, database, "MINE"); owner != "alice" {
		t.Fatalf("MINE's owner = %q, want alice, who created it", owner)
	}
	shared := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "POD", "name": "Pod's", "default_assignee": accessCarol, "actor": map[string]any{"kind": "session", "id": "ses_pod"},
	}, "agent-token")
	if shared.Code != http.StatusCreated {
		t.Fatalf("the shared token's create: %d %s", shared.Code, shared.Body.String())
	}
	if owner := projectOwnerOf(t, database, "POD"); owner != accessCarol {
		t.Fatalf("POD's owner = %q, want its default assignee %s", owner, accessCarol)
	}

	// A credential that acts for no person makes the project its default assignee's, so a
	// restricted member named as the default would own it: refused, and nothing is stored.
	before = allEvents(t, database)
	wantRefusal(t, agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "RES", "name": "Restricted's", "default_assignee": accessErin, "actor": map[string]any{"kind": "session", "id": "ses_pod"},
	}, "agent-token"), http.StatusBadRequest, "OWNER_NOT_ALLOWED", "the shared token", "POST /api/v1/projects naming a restricted member")
	if got := countRows(t, database, `select count(*) from projects where key = 'RES'`); got != 0 || allEvents(t, database) != before {
		t.Fatalf("a refused create stored %d projects and moved events from %d to %d", got, before, allEvents(t, database))
	}
}

func projectOwnerOf(t *testing.T, database *store.Store, key string) string {
	t.Helper()
	var owner string
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(owner, '') from projects where key = $1`, key).Scan(&owner); err != nil {
		t.Fatalf("read %s's owner: %v", key, err)
	}
	return owner
}

// TestACreateSetsOnlyAnOwnerSetOwnerWouldTake: one owner rule, at a create as at every later
// write. A broker token can act for a person no people row holds, whom SetOwner refuses as an
// owner, so a create acting for one leaves the owner to its default assignee, held to the full-
// member check a credential acting for nobody is: a full member's default owns the project, a
// restricted one is 400 OWNER_NOT_ALLOWED with nothing written, and naming none is
// 400 ASSIGNEE_NOT_ALLOWED, since that person is the default and has not signed in. Every owner a
// create sets is one SetOwner takes.
func TestACreateSetsOnlyAnOwnerSetOwnerWouldTake(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	seedPeople(t, rig.database, accessCarol, accessErin)
	markRestricted(t, rig.database, accessErin)
	const nobody = "nobody@outside.example"
	tokens := map[string]string{nobody: "token-nobody", brokerTestPerson: "token-alice"}
	for person, token := range tokens {
		answer := liveNamedToken(caller.jkt)
		answer.Person = person
		answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
		rig.broker.Says(token, answer)
	}
	create := func(token, key string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["key"], body["name"] = key, key
		return rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/projects", tokens[token], body))
	}

	// (a) A person no people row holds never becomes an owner.
	if response := create(nobody, "NOROW", map[string]any{"default_assignee": accessCarol}); response.Code != http.StatusCreated {
		t.Fatalf("%s's create naming %s: %d %s", nobody, accessCarol, response.Code, response.Body.String())
	}
	if owner := projectOwnerOf(t, rig.database, "NOROW"); owner != accessCarol {
		t.Fatalf("NOROW's owner = %q, want its default assignee %s: %s has never signed in, so SetOwner would refuse them", owner, accessCarol, nobody)
	}
	before := allEvents(t, rig.database)
	wantRefusal(t, create(nobody, "NORES", map[string]any{"default_assignee": accessErin}),
		http.StatusBadRequest, "OWNER_NOT_ALLOWED", nobody, "POST /api/v1/projects naming a restricted member")
	wantRefusal(t, create(nobody, "NONE", map[string]any{}),
		http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", nobody, "POST /api/v1/projects naming no default")
	if got := countRows(t, rig.database, `select count(*) from projects where key in ('NORES', 'NONE')`); got != 0 || allEvents(t, rig.database) != before {
		t.Fatalf("the refused creates stored %d projects and moved events from %d to %d", got, before, allEvents(t, rig.database))
	}
	// A full member acting for themself still owns what they create.
	if response := create(brokerTestPerson, "MINE", map[string]any{}); response.Code != http.StatusCreated {
		t.Fatalf("%s's create: %d %s", brokerTestPerson, response.Code, response.Body.String())
	}
	if owner := projectOwnerOf(t, rig.database, "MINE"); owner != brokerTestPerson {
		t.Fatalf("MINE's owner = %q, want %s, who created it", owner, brokerTestPerson)
	}

	// (b) SetOwner takes every owner a create set.
	for _, key := range []string{"NOROW", "MINE"} {
		owner := projectOwnerOf(t, rig.database, key)
		tx, err := rig.database.Pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, _, err = access.SetOwner(context.Background(), tx, events.NewBroker(), key, owner, model.Actor{Kind: "user", ID: owner})
		_ = tx.Rollback(context.Background())
		if err != nil {
			t.Errorf("SetOwner(%s, %s), the owner the create set: %v", key, owner, err)
		}
	}
}

// TestWhoamiOfAFullMemberIsUnchanged (R9): GET /api/v1/whoami answers a full member and the shared
// token byte for byte as before member kinds existed, and adds member_kind for a restricted member
// alone.
func TestWhoamiOfAFullMemberIsUnchanged(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessErin)
	markRestricted(t, database, accessErin)
	for _, want := range []struct{ who, body string }{
		{"alice", `{"kind":"user","login":"alice","scopes":["person","approve"]}`},
		{accessErin, `{"kind":"user","login":"` + accessErin + `","member_kind":"restricted","scopes":["person","approve"]}`},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, want.who)
		if got := strings.TrimSpace(response.Body.String()); response.Code != http.StatusOK || got != want.body {
			t.Errorf("whoami as %s = %d %s, want %s", want.who, response.Code, got, want.body)
		}
	}
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "agent-token")
	if got, want := strings.TrimSpace(response.Body.String()), `{"kind":"agent","owner":null,"project":null,"scopes":[],"service":null,"session":null}`; got != want {
		t.Errorf("whoami as the shared token = %s, want %s", got, want)
	}
}
