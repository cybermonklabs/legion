package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A project's proven-sessions setting on /api/v2 is the proven_sessions member of PATCH
// /api/v2/projects/{key} (R1.7: a field of an item is a member of its PATCH): true is the write PUT
// /api/v1/projects/{key}/proven-sessions makes and false the one its DELETE makes
// (writeProjectProvenSessions), and every /api/v2 read of a project row answers it.

// v2ListedProvenSessions is project's proven_sessions as GET /api/v2/projects lists it.
func v2ListedProvenSessions(t *testing.T, handler http.Handler, project string) bool {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v2/projects?limit=200", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list projects on /api/v2: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, row := range decodeBody[page[provenSessionsRow]](t, response).Items {
		if row.Key != project {
			continue
		}
		if row.ProvenSessions == nil {
			t.Fatalf("GET /api/v2/projects lists %s with no proven_sessions: %s", project, response.Body.String())
		}
		return *row.ProvenSessions
	}
	t.Fatalf("project %s is not in the /api/v2 list: %s", project, response.Body.String())
	return false
}

// TestV2PatchWritesAndRemovesTheProvenSessionsSetting: PATCH /api/v2/projects/{key} with
// proven_sessions true from the shared token turns the setting on as PUT
// /api/v1/projects/{key}/proven-sessions does - the answer, GET /api/v2/projects and GET
// /api/v1/projects all read it on, and one project.updated names the session that wrote it - and a
// second such patch changes nothing and appends nothing. proven_sessions false from the shared
// token is 403 OAUTH_SCOPE_REQUIRED and leaves it on, as the DELETE refuses that token; from a
// personal token holding the person scope it turns the setting off as the DELETE does, and its
// event names the person. A PUT on /api/v1 and a patch on /api/v2 read back the same setting in
// both versions.
func TestV2PatchWritesAndRemovesTheProvenSessionsSetting(t *testing.T) {
	rig := newBrokerTokenRig(t)
	const project, writer = "PROVENV2", "ses-turns-proven-sessions-on-v2"
	path := "/api/v2/projects/" + project
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "Proven sessions on /api/v2"}); created.Code != http.StatusCreated {
		t.Fatalf("a person's create of %s: status=%d body=%s", project, created.Code, created.Body.String())
	}
	requireSetting := func(t *testing.T, what string, want bool, updates int) {
		t.Helper()
		if listed := v2ListedProvenSessions(t, rig.handler, project); listed != want {
			t.Errorf("%s: GET /api/v2/projects lists %s with proven_sessions %t, want %t", what, project, listed, want)
		}
		if listed := listedProvenSessions(t, rig.handler, project); listed != want {
			t.Errorf("%s: GET /api/v1/projects lists %s with proven_sessions %t, want %t", what, project, listed, want)
		}
		if count := projectUpdates(t, rig.database, project); count != updates {
			t.Errorf("%s: %s holds %d project.updated events, want %d", what, project, count, updates)
		}
	}
	requireWriter := func(t *testing.T, what string, recorded model.Actor, want bool) {
		t.Helper()
		update := latestProjectUpdate(t, rig.database, project)
		if update.actor.Kind != recorded.Kind || update.actor.ID != recorded.ID {
			t.Errorf("%s: the project.updated event records %s:%s, want %s:%s", what, update.actor.Kind, update.actor.ID, recorded.Kind, recorded.ID)
		}
		if update.provenSessions == nil || *update.provenSessions != want {
			t.Errorf("%s: the project.updated event carries proven_sessions %v, want %t", what, update.provenSessions, want)
		}
	}
	requireSetting(t, "a new project", false, 0)

	turnOn := func(t *testing.T) *httptest.ResponseRecorder {
		return rig.serve(brokerRequest(t, http.MethodPatch, path, map[string]any{
			"proven_sessions": true, "actor": map[string]any{"kind": "session", "id": writer},
		}, "Bearer agent-token", ""))
	}
	if !answeredProvenSessions(t, "the shared token's patch of proven_sessions true", http.StatusOK, turnOn(t)) {
		t.Errorf("the shared token's patch of proven_sessions true answers proven_sessions false, want true")
	}
	requireSetting(t, "after the shared token's patch", true, 1)
	requireWriter(t, "the shared token's patch", model.Actor{Kind: "session", ID: writer}, true)
	if !answeredProvenSessions(t, "the shared token's second patch", http.StatusOK, turnOn(t)) {
		t.Errorf("the shared token's second patch answers proven_sessions false, want true")
	}
	requireSetting(t, "after the shared token's second patch", true, 1)

	refused := rig.serve(brokerRequest(t, http.MethodPatch, path, map[string]any{
		"proven_sessions": false, "actor": map[string]any{"kind": "session", "id": writer},
	}, "Bearer agent-token", ""))
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != apicode.OAuthScopeRequired.Name() {
		t.Errorf("the shared token's patch of proven_sessions false: status=%d body=%s, want 403 %s",
			refused.Code, refused.Body.String(), apicode.OAuthScopeRequired.Name())
	}
	requireSetting(t, "after the shared token's patch of proven_sessions false", true, 1)

	personal := mintPersonalToken(t, rig.handler, brokerTestPerson, "a person-scoped token", []string{model.ScopePerson}).Token
	turnOff := rig.serve(brokerRequest(t, http.MethodPatch, path, map[string]any{"proven_sessions": false}, "Bearer "+personal, ""))
	if answeredProvenSessions(t, "the personal token's patch of proven_sessions false", http.StatusOK, turnOff) {
		t.Errorf("the personal token's patch of proven_sessions false answers proven_sessions true, want false")
	}
	requireSetting(t, "after the personal token's patch", false, 2)
	requireWriter(t, "the personal token's patch", model.Actor{Kind: "user", ID: brokerTestPerson}, false)

	// The /api/v1 PUT's write is the one the /api/v2 patch reads, and the reverse.
	if on := rig.serve(brokerRequest(t, http.MethodPut, "/api/v1/projects/"+project+"/proven-sessions", map[string]any{
		"actor": map[string]any{"kind": "session", "id": writer},
	}, "Bearer agent-token", "")); on.Code != http.StatusOK {
		t.Fatalf("the shared token's PUT on /api/v1: status=%d body=%s", on.Code, on.Body.String())
	}
	requireSetting(t, "after the shared token's PUT on /api/v1", true, 3)
	if answeredProvenSessions(t, "the personal token's second patch of proven_sessions false", http.StatusOK,
		rig.serve(brokerRequest(t, http.MethodPatch, path, map[string]any{"proven_sessions": false}, "Bearer "+personal, ""))) {
		t.Errorf("the personal token's second patch answers proven_sessions true, want false")
	}
	requireSetting(t, "after the personal token's second patch", false, 4)
}
