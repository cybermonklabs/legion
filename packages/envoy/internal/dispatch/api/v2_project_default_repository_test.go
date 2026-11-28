package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// A project's default repository on /api/v2 is the default_repository member of PATCH
// /api/v2/projects/{key} (R1.7: a field of an item is a member of its PATCH), the write PUT
// /api/v1/projects/{key}/default-repository makes (#4457, writeProjectDefaultRepository), and every
// /api/v2 read of a project row answers it: GET /api/v2/projects and the PATCH's own answer.

// patchDefaultRepositoryV2 sends PATCH /api/v2/projects/{project} naming default_repository alone,
// value an `owner/name` string, nil for null, or any other JSON value, as a person.
func patchDefaultRepositoryV2(t *testing.T, handler http.Handler, project string, value any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v2/projects/"+project, map[string]any{"default_repository": value}, "alice")
}

// v2ProjectMembers is one project's members as GET /api/v2/projects lists them, member by member,
// so a test tells a member that is absent from one that is null.
func v2ProjectMembers(t *testing.T, handler http.Handler, project string) map[string]json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/projects?limit=200", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v2/projects: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, row := range decodeBody[page[map[string]json.RawMessage]](t, response).Items {
		if string(row["key"]) == `"`+project+`"` {
			return row
		}
	}
	t.Fatalf("project %s is not in GET /api/v2/projects: %s", project, response.Body.String())
	return nil
}

// patchedDefaultRepository is the default_repository member of a PATCH's 200 answer, and whether
// the member is there at all.
func patchedDefaultRepository(t *testing.T, what string, response *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s, want 200", what, response.Code, response.Body.String())
	}
	raw, present := decodeBody[map[string]json.RawMessage](t, response)["default_repository"]
	return string(raw), present
}

// "owner/name" sets the default, in the canonical form an issue's own repository takes, and the
// project read and the PATCH's answer carry it as default_repository, never as /api/v1's
// default_repo; a project with none carries no member. The value the project holds already,
// in another spelling, changes nothing and appends no event, and a change appends one
// project.updated. null clears it, and a second null appends nothing. A patch that names other
// members and not default_repository leaves it as it stands.
func TestV2PatchSetsAndClearsAProjectsDefaultRepository(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE", "BARE")

	if got, present := patchedDefaultRepository(t, "the set", patchDefaultRepositoryV2(t, handler, "CORE", "Acme/Widgets.git")); !present || got != `"acme/widgets"` {
		t.Fatalf("the set answered default_repository %s (present %v), want \"acme/widgets\"", got, present)
	}
	core := v2ProjectMembers(t, handler, "CORE")
	if got := string(core["default_repository"]); got != `"acme/widgets"` {
		t.Fatalf("GET /api/v2/projects lists CORE's default_repository %s, want \"acme/widgets\": %v", got, core)
	}
	if _, present := core["default_repo"]; present {
		t.Fatalf("GET /api/v2/projects lists CORE with /api/v1's default_repo member: %v", core)
	}
	if got, present := v2ProjectMembers(t, handler, "BARE")["default_repository"]; present {
		t.Fatalf("GET /api/v2/projects lists BARE's default_repository %s, want no member for a project with none", got)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("the set appended %d project events, want one", updates)
	}
	if event := lastProjectUpdate(t, database, "CORE"); string(event["default_repo"]) != `"acme/widgets"` {
		t.Fatalf("the set's project event carries %v, want CORE's row holding acme/widgets", event)
	}

	if got, _ := patchedDefaultRepository(t, "the same value again", patchDefaultRepositoryV2(t, handler, "CORE", "acme/widgets")); got != `"acme/widgets"` {
		t.Fatalf("the unchanged patch answered default_repository %s, want \"acme/widgets\"", got)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("the same value again left %d project events, want the set's one", updates)
	}

	if got, present := patchedDefaultRepository(t, "the clear", patchDefaultRepositoryV2(t, handler, "CORE", nil)); present {
		t.Fatalf("the clear answered default_repository %s, want no member", got)
	}
	if got, present := v2ProjectMembers(t, handler, "CORE")["default_repository"]; present {
		t.Fatalf("GET /api/v2/projects lists CORE's default_repository %s after the clear, want no member", got)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 2 {
		t.Fatalf("the clear left %d project events, want two", updates)
	}
	patchedDefaultRepository(t, "a second clear", patchDefaultRepositoryV2(t, handler, "CORE", nil))
	if updates := projectUpdates(t, database, "CORE"); updates != 2 {
		t.Fatalf("a second clear left %d project events, want the two already there", updates)
	}

	patchedDefaultRepository(t, "a set before a patch of the gated steps", patchDefaultRepositoryV2(t, handler, "CORE", "acme/gadgets"))
	if got, _ := patchedDefaultRepository(t, "a patch of the gated steps alone", dispatchRequest(t, handler, http.MethodPatch,
		"/api/v2/projects/CORE", map[string]any{"gated_steps": []string{"spec"}}, "alice")); got != `"acme/gadgets"` {
		t.Fatalf("a patch of the gated steps alone answered default_repository %s, want \"acme/gadgets\" as it stood", got)
	}
	if updates := projectUpdates(t, database, "BARE"); updates != 0 {
		t.Fatalf("BARE holds %d project events, want none: no write named it", updates)
	}
}

// A default_repository that is not a repository GitHub could hold, or no string and no null, is
// refused 400 INVALID_PROJECT naming default_repository, the code PATCH /api/v2/projects/{key}
// answers a body naming none of its fields, a blank default_assignee, a bad gated_steps and a bad
// default_repository with, and the one PUT /api/v1/projects/{key}/default-repository answers for
// the same values; the project keeps the default it had and no event is appended.
func TestV2PatchRefusesADefaultRepositoryThatIsNoRepository(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "CORE")
	patchedDefaultRepository(t, "the set", patchDefaultRepositoryV2(t, handler, "CORE", "acme/widgets"))

	for name, value := range map[string]any{
		"a name with no owner":  "acme",
		"a name with two slash": "a/b/c",
		"a path out of a tree":  "../x",
		"a dot name":            "acme/..",
		"a number":              42,
	} {
		response := patchDefaultRepositoryV2(t, handler, "CORE", value)
		code, text := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != apicode.InvalidProject.Name() {
			t.Errorf("%s: status=%d body=%s, want 400 %s", name, response.Code, response.Body.String(), apicode.InvalidProject.Name())
		}
		if !strings.Contains(text, "default_repository") {
			t.Errorf("%s: the refusal %q does not name default_repository", name, text)
		}
	}
	if got := string(v2ProjectMembers(t, handler, "CORE")["default_repository"]); got != `"acme/widgets"` {
		t.Fatalf("GET /api/v2/projects lists CORE's default_repository %s after the refusals, want \"acme/widgets\"", got)
	}
	if updates := projectUpdates(t, database, "CORE"); updates != 1 {
		t.Fatalf("the refusals left %d project events, want the set's one", updates)
	}
}

// The default belongs to the deployment on /api/v2 as on /api/v1
// (TestTheDefaultRepositoryIsADeploymentWrite): a credential that may write one project is refused
// a patch naming default_repository, 403 PROJECT_SCOPE_DEPLOYMENT, though the path names that very
// project, and nothing is written. The credential the deployment configured writes it.
func TestV2PatchRefusesAProjectScopedCredentialTheDefaultRepository(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-token-that-may-not-move-a-projects-default-repository-on-v2"
	rig.broker.Says(token, liveNamedToken(caller.jkt))
	seedProject(t, rig, brokerTestProject, "Broker")
	path := "/api/v2/projects/" + brokerTestProject

	refused := rig.serve(caller.presenting(t, http.MethodPatch, path, token, map[string]any{"default_repository": "acme/widgets"}))
	if code, text := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != apicode.ProjectScopeDeployment.Name() ||
		!strings.Contains(text, brokerTestProject) || !strings.Contains(text, "default_repository") {
		t.Fatalf("a project-scoped token: status=%d body=%s, want 403 %s naming %s and default_repository",
			refused.Code, refused.Body.String(), apicode.ProjectScopeDeployment.Name(), brokerTestProject)
	}
	if got, present := v2ProjectMembers(t, rig.handler, brokerTestProject)["default_repository"]; present {
		t.Fatalf("the refused patch left default_repository %s, want none", got)
	}

	deployment := agentRequest(t, rig.handler, http.MethodPatch, path,
		map[string]any{"default_repository": "acme/gadgets", "actor": sessionActor()}, "agent-token")
	if got, _ := patchedDefaultRepository(t, "the deployment's token", deployment); got != `"acme/gadgets"` {
		t.Fatalf("the deployment's token answered default_repository %s, want \"acme/gadgets\"", got)
	}
}
