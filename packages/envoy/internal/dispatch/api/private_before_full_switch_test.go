package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// requireBeforeFullSwitch fails unless response is 200 answering key's access private with
// private_before_full_switch true, key is private in the database, and the switch appended one
// event, project.access_changed, past before.
func requireBeforeFullSwitch(t *testing.T, database *store.Store, response *httptest.ResponseRecorder, key string, before int64) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH %s private before its full switch = %d %s, want 200", key, response.Code, response.Body.String())
	}
	answer := decodeBody[model.ProjectAccess](t, response)
	if answer.Visibility != model.ProjectPrivate || !answer.PrivateBeforeFullSwitch || !strings.Contains(response.Body.String(), `"private_before_full_switch":true`) {
		t.Fatalf("PATCH %s private before its full switch answered %s, want it private with private_before_full_switch true", key, response.Body.String())
	}
	if visibility := visibilityOf(t, database, key); visibility != model.ProjectPrivate {
		t.Fatalf("PATCH %s private before its full switch left it %s, want private", key, visibility)
	}
	var appended []string
	rows, err := database.Pool.Query(context.Background(), `select type from events order by id offset $1`, before)
	if err != nil {
		t.Fatalf("read the events the switch appended: %v", err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("read an event the switch appended: %v", err)
		}
		appended = append(appended, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events the switch appended: %v", err)
	}
	if !slices.Equal(appended, []string{"project.access_changed"}) {
		t.Fatalf("PATCH %s private before its full switch appended %v, want one project.access_changed", key, appended)
	}
}

// TestAProjectBecomesPrivateBeforeItsFullSwitch (spec "Day one and the full switch"): a project
// DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names goes private with no bus to write its record and no
// provisioning, appends project.access_changed as any switch does, and answers its access with
// private_before_full_switch true; no writer is asked for its record, even one the deployment has.
// Its store must still be complete: each row of the table a full switch is refused by
// (requireAnIncompleteStoreRefusesTheSwitch) refuses it 409 PRIVATE_PROJECTS_INCOMPLETE as for any
// project. Making it public again is the plain switch to public. A project on neither list keeps
// 409 PRIVATE_PROJECT_NOT_PROVISIONED beside the list, and a project on DISPATCH_PRIVATE_PROJECTS
// alone keeps 409 PRIVATE_PROJECTS_INCOMPLETE and 409 PRIVATE_PROJECTS_UNAVAILABLE. The two lists
// never share a project: NewDeps refuses lists that do, since the boot takes a key both settings
// name off this list (cmd/dispatch, TestThePrivateBeforeFullSwitchSettingIsAListOfKeys).
//
// Controls: let every key skip the provisioning and the record, and the rows of a project on
// neither list and on DISPATCH_PRIVATE_PROJECTS alone fail; skip the store's check for a project
// before its full switch, and the incomplete rows fail; drop NewDeps' refusal, and the row of a
// project on both lists fails.
func TestAProjectBecomesPrivateBeforeItsFullSwitch(t *testing.T) {
	private := map[string]string{"visibility": model.ProjectPrivate}
	public := map[string]string{"visibility": model.ProjectPublic}
	beforeFullSwitch := []string{"PRIV"}

	t.Run("goes private with no bus and nothing provisioned", func(t *testing.T) {
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateBeforeFullSwitch: beforeFullSwitch}, nil)
		before := allEvents(t, database)
		requireBeforeFullSwitch(t, database, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice"), "PRIV", before)
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/PRIV/access", nil, "alice")
		if read.Code != http.StatusOK || !decodeBody[model.ProjectAccess](t, read).PrivateBeforeFullSwitch {
			t.Fatalf("GET PRIV's access while it is private before its full switch = %d %s, want 200 with private_before_full_switch true", read.Code, read.Body.String())
		}
		made := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", public, "alice")
		if made.Code != http.StatusOK || visibilityOf(t, database, "PRIV") != model.ProjectPublic || strings.Contains(made.Body.String(), "private_before_full_switch") {
			t.Fatalf("PATCH PRIV public after its switch before the full one = %d %s, PRIV %s; want 200, PRIV public, and no private_before_full_switch",
				made.Code, made.Body.String(), visibilityOf(t, database, "PRIV"))
		}
	})

	t.Run("asks no writer for its record", func(t *testing.T) {
		bus := &recordingBus{}
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateBeforeFullSwitch: beforeFullSwitch}, bus)
		before := allEvents(t, database)
		requireBeforeFullSwitch(t, database, dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/PRIV/access", private, "alice"), "PRIV", before)
		if calls := bus.taken(); len(calls) != 0 {
			t.Fatalf("PRIV's switch before its full switch called the writer: %+v", calls)
		}
	})

	t.Run("still refused while the store is incomplete", func(t *testing.T) {
		requireAnIncompleteStoreRefusesTheSwitch(t, testServerOptions{privateBeforeFullSwitch: beforeFullSwitch})
	})

	t.Run("a project on neither list", func(t *testing.T) {
		for _, provisioned := range [][]string{nil, {"OTHER"}} {
			bus := &recordingBus{}
			handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: provisioned, privateBeforeFullSwitch: beforeFullSwitch}, bus)
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "NEITHER", "name": "NEITHER"}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create NEITHER: %d %s", response.Code, response.Body.String())
			}
			before := allEvents(t, database)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/NEITHER/access", private, "alice")
			requireSwitchRefused(t, database, response, "NEITHER", before, http.StatusConflict, "PRIVATE_PROJECT_NOT_PROVISIONED",
				"NEITHER", "DISPATCH_PRIVATE_PROJECTS", "DISPATCH_PRIVATE_BEFORE_FULL_SWITCH")
			if calls := bus.taken(); len(calls) != 0 {
				t.Fatalf("DISPATCH_PRIVATE_PROJECTS=%v: NEITHER's refused switch called the writer: %+v", provisioned, calls)
			}
		}
	})

	t.Run("a project on DISPATCH_PRIVATE_PROJECTS alone", func(t *testing.T) {
		handler, database := newPrivateProjectsServer(t, testServerOptions{privateProjects: []string{"OTHER"}, privateBeforeFullSwitch: beforeFullSwitch}, nil)
		before := allEvents(t, database)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice")
		requireSwitchRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", privateProjectsBucket)
		if _, err := database.Pool.Exec(context.Background(), `delete from session_projects_filled`); err != nil {
			t.Fatalf("empty session_projects_filled: %v", err)
		}
		response = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice")
		requireSwitchRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", "session_projects_filled")
	})

	t.Run("a project on both lists is refused at the start", func(t *testing.T) {
		_, err := NewDeps(DepsInput{PrivateProjects: []string{"OTHER", "PRIV"}, PrivateBeforeFullSwitch: []string{"DAYONE", "PRIV"}})
		if err == nil || !strings.Contains(err.Error(), `"PRIV"`) || strings.Contains(err.Error(), "DAYONE") ||
			!strings.Contains(err.Error(), "DISPATCH_PRIVATE_PROJECTS") || !strings.Contains(err.Error(), "DISPATCH_PRIVATE_BEFORE_FULL_SWITCH") {
			t.Fatalf("NewDeps with PRIV on both lists = %v, want it refused naming PRIV and both settings, and not DAYONE", err)
		}
		if _, err := NewDeps(withDocumentService(t, DepsInput{PrivateProjects: []string{"OTHER"}, PrivateBeforeFullSwitch: beforeFullSwitch})); err != nil {
			t.Fatalf("NewDeps with lists that share no project = %v, want them taken", err)
		}
	})
}

// beforeFullSwitchRoster is two sessions that have written nothing: session-priv registered with a
// token that lists PRIV alone, and session-pub with one that lists PUB alone.
const beforeFullSwitchRoster = `[
	{"session_id":"session-priv","title":"On PRIV","capabilities":["btw"],"projects":["PRIV"],"last_seen":200},
	{"session_id":"session-pub","title":"On PUB","capabilities":["btw"],"projects":["PUB"],"last_seen":100}
]`
