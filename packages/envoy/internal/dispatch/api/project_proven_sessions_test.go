// project_proven_sessions_test.go holds the behavior tests of a project's proven-sessions setting
// (project_proven_sessions.go, R2 of the spec-writer spec): a new project has it off, any
// credential that may write the project turns it on, only a person turns it off, and the
// project.updated event a gated-steps publication appends carries the setting as the row stores it.
// It also holds that the project.updated event of a proven-sessions PUT and of a gated-steps
// publication carries the project's default repository as the row holds it when the write commits.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// provenSessionsRow is a project as a route answers it. ProvenSessions is nil where the answer
// carries no proven_sessions, so an absent field is not read as a setting that is off.
type provenSessionsRow struct {
	Key            string `json:"key"`
	ProvenSessions *bool  `json:"proven_sessions"`
}

// answeredProvenSessions is the proven_sessions of the project response answers. It fails t unless
// response answered status, and when the answer carries no proven_sessions.
func answeredProvenSessions(t *testing.T, what string, status int, response *httptest.ResponseRecorder) bool {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d", what, response.Code, response.Body.String(), status)
	}
	row := decodeBody[provenSessionsRow](t, response)
	if row.ProvenSessions == nil {
		t.Fatalf("%s answers no proven_sessions: %s", what, response.Body.String())
	}
	return *row.ProvenSessions
}

// listedProvenSessions is project's proven_sessions as GET /api/v1/projects lists it. It fails t
// when the list does not hold the project, or holds it with no proven_sessions.
func listedProvenSessions(t *testing.T, handler http.Handler, project string) bool {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, row := range decodeBody[[]provenSessionsRow](t, response) {
		if row.Key != project {
			continue
		}
		if row.ProvenSessions == nil {
			t.Fatalf("GET /api/v1/projects lists %s with no proven_sessions: %s", project, response.Body.String())
		}
		return *row.ProvenSessions
	}
	t.Fatalf("project %s is not in the list: %s", project, response.Body.String())
	return false
}

// projectUpdate is a project.updated event: its writer, and the gated steps, the setting and the
// default repository of the project row its payload carries, provenSessions nil where the payload
// carries no proven_sessions and defaultRepo nil where it carries no default_repo.
type projectUpdate struct {
	actor          model.Actor
	gatedSteps     []string
	provenSessions *bool
	defaultRepo    *string
}

// latestProjectUpdate is the last project.updated event of project.
func latestProjectUpdate(t *testing.T, database *store.Store, project string) projectUpdate {
	t.Helper()
	var actor, payload []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select actor, payload from events where project_key = $1 and type = 'project.updated'
		order by id desc limit 1
	`, project).Scan(&actor, &payload); err != nil {
		t.Fatalf("read the last project.updated event of %s: %v", project, err)
	}
	var update projectUpdate
	if err := json.Unmarshal(actor, &update.actor); err != nil {
		t.Fatalf("decode the actor %s of %s's project.updated event: %v", actor, project, err)
	}
	var row struct {
		GatedSteps     []string `json:"gated_steps"`
		ProvenSessions *bool    `json:"proven_sessions"`
		DefaultRepo    *string  `json:"default_repo"`
	}
	if err := json.Unmarshal(payload, &row); err != nil {
		t.Fatalf("decode the payload %s of %s's project.updated event: %v", payload, project, err)
	}
	update.gatedSteps, update.provenSessions, update.defaultRepo = row.GatedSteps, row.ProvenSessions, row.DefaultRepo
	return update
}

// TestSpecWriteProvenSessionsSettingStartsOffAndOnlyAPersonTurnsItOff: a new project answers
// proven_sessions false on its create and on GET /api/v1/projects. The shared token's PUT turns the
// setting on and appends project.updated naming the session that wrote it; a second PUT changes
// nothing and appends nothing. A personal token with the person scope turns it off, and its event
// names the person. A broker token scoped to another project is refused by the project-scope
// layer, 403 PROJECT_SCOPE, and one scoped to the project turns the setting on. The shared token's
// DELETE and the project-scoped broker token's DELETE are 403 OAUTH_SCOPE_REQUIRED and leave it
// on; the personal token's DELETE turns it off again, and its event names the person.
func TestSpecWriteProvenSessionsSettingStartsOffAndOnlyAPersonTurnsItOff(t *testing.T) {
	rig := newBrokerTokenRig(t)
	const project, writer = "PROVEN", "ses-turns-proven-sessions-on"
	path := "/api/v1/projects/" + project + "/proven-sessions"
	created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": "Proven sessions"})
	if answeredProvenSessions(t, "a person's create of "+project, http.StatusCreated, created) {
		t.Errorf("the create of %s answers proven_sessions true, want false", project)
	}
	// requireSetting fails t unless the list shows the setting as want, and the project holds
	// updates project.updated events.
	requireSetting := func(t *testing.T, what string, want bool, updates int) {
		t.Helper()
		if listed := listedProvenSessions(t, rig.handler, project); listed != want {
			t.Errorf("%s: GET /api/v1/projects lists %s with proven_sessions %t, want %t", what, project, listed, want)
		}
		if count := projectUpdates(t, rig.database, project); count != updates {
			t.Errorf("%s: %s holds %d project.updated events, want %d", what, project, count, updates)
		}
	}
	// requireWriter fails t unless the last project.updated event of the project records recorded
	// and carries the setting as want.
	requireWriter := func(t *testing.T, what string, recorded model.Actor, want bool) {
		t.Helper()
		update := latestProjectUpdate(t, rig.database, project)
		if update.actor.Kind != recorded.Kind || update.actor.ID != recorded.ID || update.actor.Proven != recorded.Proven {
			t.Errorf("%s: the project.updated event records %s:%s proven=%t, want %s:%s proven=%t", what,
				update.actor.Kind, update.actor.ID, update.actor.Proven, recorded.Kind, recorded.ID, recorded.Proven)
		}
		if update.provenSessions == nil || *update.provenSessions != want {
			t.Errorf("%s: the project.updated event carries proven_sessions %v, want %t", what, update.provenSessions, want)
		}
	}
	requireSetting(t, "a new project", false, 0)

	turnOn := func(t *testing.T) *httptest.ResponseRecorder {
		return rig.serve(brokerRequest(t, http.MethodPut, path, map[string]any{
			"actor": map[string]any{"kind": "session", "id": writer},
		}, "Bearer agent-token", ""))
	}
	if !answeredProvenSessions(t, "the shared token's PUT", http.StatusOK, turnOn(t)) {
		t.Errorf("the shared token's PUT answers proven_sessions false, want true")
	}
	requireSetting(t, "after the shared token's PUT", true, 1)
	requireWriter(t, "the shared token's PUT", model.Actor{Kind: "session", ID: writer}, true)
	if !answeredProvenSessions(t, "the shared token's second PUT", http.StatusOK, turnOn(t)) {
		t.Errorf("the shared token's second PUT answers proven_sessions false, want true")
	}
	requireSetting(t, "after the shared token's second PUT", true, 1)

	personal := mintPersonalToken(t, rig.handler, brokerTestPerson, "a person-scoped token", []string{model.ScopePerson}).Token
	turnOff := func(t *testing.T) *httptest.ResponseRecorder {
		return rig.serve(brokerRequest(t, http.MethodDelete, path, nil, "Bearer "+personal, ""))
	}
	person := model.Actor{Kind: "user", ID: brokerTestPerson, Proven: true}
	if answeredProvenSessions(t, "the personal token's DELETE", http.StatusOK, turnOff(t)) {
		t.Errorf("the personal token's DELETE answers proven_sessions true, want false")
	}
	requireSetting(t, "after the personal token's DELETE", false, 2)
	requireWriter(t, "the personal token's DELETE", person, false)

	caller := newBrokerCaller(t)
	const own, other = "a-token-scoped-to-the-project", "a-token-scoped-to-another-project"
	for token, scoped := range map[string]string{own: project, other: "OTHER"} {
		says := liveNamedToken(caller.jkt)
		says.Projects = []string{scoped}
		rig.broker.Says(token, says)
	}
	refused := rig.serve(caller.presenting(t, http.MethodPut, path, other, nil))
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != codeProjectScope {
		t.Errorf("the PUT of a broker token scoped to OTHER: status=%d body=%s, want 403 %s", refused.Code, refused.Body.String(), codeProjectScope)
	}
	requireSetting(t, "after the PUT of a broker token scoped to OTHER", false, 2)
	if !answeredProvenSessions(t, "the PUT of a broker token scoped to "+project, http.StatusOK, rig.serve(caller.presenting(t, http.MethodPut, path, own, nil))) {
		t.Errorf("the PUT of a broker token scoped to %s answers proven_sessions false, want true", project)
	}
	requireSetting(t, "after the PUT of a broker token scoped to "+project, true, 3)
	requireWriter(t, "the PUT of a broker token scoped to "+project, model.Actor{Kind: "session", ID: brokerTestSession, Proven: true}, true)

	for _, refusal := range []struct {
		what string
		send func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"the shared token's DELETE", func(t *testing.T) *httptest.ResponseRecorder {
			return rig.serve(brokerRequest(t, http.MethodDelete, path, nil, "Bearer agent-token", ""))
		}},
		{"the DELETE of a broker token scoped to " + project, func(t *testing.T) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, http.MethodDelete, path, own, nil))
		}},
	} {
		response := refusal.send(t)
		if code, _ := refusalOf(t, response); response.Code != http.StatusForbidden || code != codeScopeRequired {
			t.Errorf("%s: status=%d body=%s, want 403 %s", refusal.what, response.Code, response.Body.String(), codeScopeRequired)
		}
		requireSetting(t, "after "+refusal.what, true, 3)
	}

	if answeredProvenSessions(t, "the personal token's second DELETE", http.StatusOK, turnOff(t)) {
		t.Errorf("the personal token's second DELETE answers proven_sessions true, want false")
	}
	requireSetting(t, "after the personal token's second DELETE", false, 4)
	requireWriter(t, "the personal token's second DELETE", person, false)
}

// TestSpecWriteAGatedStepsEventCarriesTheStoredSetting: the project.updated event a gated-steps
// publication appends carries the project's proven_sessions as the row stores it: false before the
// setting is turned on, and true after it.
func TestSpecWriteAGatedStepsEventCarriesTheStoredSetting(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	const project = "PROVEN"
	createAdviceProject(t, handler, project)
	publishGatedSteps(t, handler, project, []string{model.GatedStepSpec})
	if update := latestProjectUpdate(t, database, project); update.provenSessions == nil || *update.provenSessions {
		t.Errorf("with the setting off, the gated-steps event carries proven_sessions %v, want false", update.provenSessions)
	}
	if on := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/proven-sessions",
		map[string]any{"actor": sessionActor()}, "agent-token"); on.Code != http.StatusOK {
		t.Fatalf("turn %s's setting on: status=%d body=%s", project, on.Code, on.Body.String())
	}
	steps := []string{model.GatedStepCriteria, model.GatedStepSpec}
	publishGatedSteps(t, handler, project, steps)
	update := latestProjectUpdate(t, database, project)
	if !slices.Equal(update.gatedSteps, steps) {
		t.Fatalf("the last project.updated event carries the gated steps %v, want the %v just published", update.gatedSteps, steps)
	}
	if update.provenSessions == nil || !*update.provenSessions {
		t.Errorf("with the setting on, the gated-steps event carries proven_sessions %v, want true", update.provenSessions)
	}
}

// TestAProjectUpdateCarriesTheDefaultRepositoryItsWriteLeft: the project.updated event of a
// proven-sessions PUT and of a gated-steps publication carries the project's default_repo as the
// row holds it when the write commits: the write reads the row under the project's owner lock,
// which every writer of the row takes first. Each write here waits on that lock, which the test
// holds while it sets the default repository; once the lock is released, the write's event names
// the repository set while it waited.
func TestAProjectUpdateCarriesTheDefaultRepositoryItsWriteLeft(t *testing.T) {
	for _, write := range []struct {
		name string
		body map[string]any
		path string
	}{
		{"a proven-sessions PUT", map[string]any{"actor": sessionActor()}, "/proven-sessions"},
		{"a gated-steps publication", map[string]any{"steps": []string{model.GatedStepSpec}, "actor": sessionActor()}, "/gated-steps"},
	} {
		t.Run(write.name, func(t *testing.T) {
			ctx := context.Background()
			handler, database := newTestHandlerWithStore(t)
			const project = "REPO"
			createAdviceProject(t, handler, project)

			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the holder: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if err := store.LockOwner(ctx, holder, store.ProjectOwner(project)); err != nil {
				t.Fatalf("hold %s's owner lock: %v", project, err)
			}
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				answered <- agentRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+write.path, write.body, "agent-token")
			}()
			if early, answeredEarly, _ := storetest.AwaitOwnerLockWaiter(t, database, write.name, answered); answeredEarly {
				t.Fatalf("%s answered %d %s while this test held %s's owner lock", write.name, early.Code, early.Body.String(), project)
			}
			// The default repository changes while the write waits on the project's owner lock.
			if _, err := holder.Exec(ctx, `update projects set default_repo = 'acme/widgets' where key = $1`, project); err != nil {
				t.Fatalf("set %s's default repository: %v", project, err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("release %s's owner lock: %v", project, err)
			}
			var response *httptest.ResponseRecorder
			select {
			case response = <-answered:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s never answered once the owner lock was released", write.name)
			}
			if response.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", write.name, response.Code, response.Body.String())
			}
			if update := latestProjectUpdate(t, database, project); update.defaultRepo == nil || *update.defaultRepo != "acme/widgets" {
				t.Errorf("%s's project.updated event carries default_repo %q, want acme/widgets, which the row held when it committed",
					write.name, deref(update.defaultRepo))
			}
		})
	}
}
