package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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
			requireRefused(t, database, response, "NEITHER", before, http.StatusConflict, "PRIVATE_PROJECT_NOT_PROVISIONED",
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
		requireRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECTS_UNAVAILABLE", privateProjectsBucket)
		if _, err := database.Pool.Exec(context.Background(), `delete from session_projects_filled`); err != nil {
			t.Fatalf("empty session_projects_filled: %v", err)
		}
		response = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/projects/OTHER/access", private, "alice")
		requireRefused(t, database, response, "OTHER", before, http.StatusConflict, "PRIVATE_PROJECTS_INCOMPLETE", "session_projects_filled")
	})

	t.Run("a project on both lists is refused at the start", func(t *testing.T) {
		_, err := NewDeps(DepsInput{PrivateProjects: []string{"OTHER", "PRIV"}, PrivateBeforeFullSwitch: []string{"DAYONE", "PRIV"}})
		if err == nil || !strings.Contains(err.Error(), `"PRIV"`) || strings.Contains(err.Error(), "DAYONE") ||
			!strings.Contains(err.Error(), "DISPATCH_PRIVATE_PROJECTS") || !strings.Contains(err.Error(), "DISPATCH_PRIVATE_BEFORE_FULL_SWITCH") {
			t.Fatalf("NewDeps with PRIV on both lists = %v, want it refused naming PRIV and both settings, and not DAYONE", err)
		}
		if _, err := NewDeps(DepsInput{PrivateProjects: []string{"OTHER"}, PrivateBeforeFullSwitch: beforeFullSwitch}); err != nil {
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

// TestAProjectPrivateBeforeItsFullSwitchIsHiddenAsAnyPrivateProject (spec "Day one and the full
// switch"): Dispatch hides a project private before its full switch from every person and
// credential that does not see it, as for any private project. On verification v16's seed, carol
// makes PRIV private through the visibility route twice, on two deployments: one that names PRIV
// private before its full switch and has no bus, and one that has provisioned it and has a bus
// that takes its record. On each, alice, who does not see PRIV, reads PRIV's issue and its
// document exactly as an issue and a document nobody holds, finds none of PRIV's objects by
// search, and lists no session of PRIV among the agents; carol, who sees PRIV, reads each.
// Control: the same reads with PRIV still public find PRIV's objects and session for alice, so
// each read can see what it is looking for.
func TestAProjectPrivateBeforeItsFullSwitchIsHiddenAsAnyPrivateProject(t *testing.T) {
	for _, deployment := range []struct {
		name    string
		options testServerOptions
		flagged bool
	}{
		{"private before its full switch", testServerOptions{privateBeforeFullSwitch: []string{seedPrivate}}, true},
		{"provisioned", testServerOptions{privateProjects: []string{seedPrivate}, busProjects: acceptingBus{}}, false},
	} {
		t.Run(deployment.name, func(t *testing.T) {
			listener, _ := newBroadcastListener(t, beforeFullSwitchRoster)
			embedder := &fakeEmbedder{vectors: map[string][]float32{searchAccessMeaning: queryVector(), searchAccessWord: angledVector(0, 3)}}
			options := deployment.options
			options.envoyURL, options.embedder = listener.URL, embedder
			seed := seedPublicProjectsWith(t, options)
			objects := writeSearchableObjects(t, seed, embedder)
			document := strings.TrimPrefix(objects[seedPrivate][1], "document/")
			issue := seed.issue[seedPrivate]

			agents := func(caller privateCaller) []string {
				t.Helper()
				got := []string{}
				for _, row := range decodeBody[[]struct {
					SessionID string `json:"session_id"`
				}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/agents", nil, http.StatusOK)) {
					got = append(got, row.SessionID)
				}
				slices.Sort(got)
				return got
			}
			found := func(caller privateCaller) []string {
				t.Helper()
				return searchResultKeys(seed.searchAs(t, caller, "q="+searchAccessWord+"&keywords_only=true").Results)
			}
			every := slices.Sorted(slices.Values(slices.Concat(objects[seedPublic], objects[seedPrivate])))
			// The control: while PRIV is public, alice finds and lists what the reads below must hide.
			if got := found(callerAlice); !slices.Equal(got, every) {
				t.Fatalf("alice's search while PRIV is public finds %v, want every object %v: the search could not see PRIV's", got, every)
			}
			if got := agents(callerAlice); !slices.Equal(got, []string{"session-priv", "session-pub"}) {
				t.Fatalf("alice's agents while PRIV is public = %v, want both sessions: the list could not see PRIV's", got)
			}

			if _, err := seed.database.Pool.Exec(context.Background(), `
				insert into session_projects_filled default values;
				insert into access_events_scrubbed default values;
			`); err != nil {
				t.Fatalf("write the backfill markers: %v", err)
			}
			switched := seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/projects/"+seedPrivate+"/access",
				map[string]string{"visibility": model.ProjectPrivate}, http.StatusOK)
			if flagged := decodeBody[model.ProjectAccess](t, switched).PrivateBeforeFullSwitch; flagged != deployment.flagged {
				t.Fatalf("carol's switch of PRIV answered %s: private_before_full_switch %v, want %v", switched.Body.String(), flagged, deployment.flagged)
			}

			for _, read := range []struct {
				what, present, absent string
			}{
				{"PRIV's issue", "/api/v1/issues/" + issue, "/api/v1/issues/" + seedPrivate + "-999999"},
				{"PRIV's document", "/api/v1/artifacts/" + document, "/api/v1/artifacts/" + uuid.NewString()},
			} {
				got := seed.request(t, callerAlice, http.MethodGet, read.present, nil)
				missing := seed.request(t, callerAlice, http.MethodGet, read.absent, nil)
				absentID, presentID := read.absent[strings.LastIndex(read.absent, "/")+1:], read.present[strings.LastIndex(read.present, "/")+1:]
				want := missingAnswerOf(missing.Body.Bytes(), map[string]string{"{id}": absentID}, map[string]string{"{id}": presentID})
				if got.Code != missing.Code || string(got.Body.Bytes()) != string(want) {
					t.Errorf("alice reads %s at GET %s = %d %s, want it as GET %s answers, %d %s",
						read.what, read.present, got.Code, got.Body.String(), read.absent, missing.Code, want)
				}
				if seen := seed.request(t, callerCarol, http.MethodGet, read.present, nil); seen.Code != http.StatusOK {
					t.Errorf("carol, who sees PRIV, reads %s at GET %s = %d %s, want 200", read.what, read.present, seen.Code, seen.Body.String())
				}
			}
			if got, want := found(callerAlice), slices.Sorted(slices.Values(objects[seedPublic])); !slices.Equal(got, want) {
				t.Errorf("alice's search with PRIV private finds %v, want PUB's objects alone, %v", got, want)
			}
			if got := found(callerCarol); !slices.Equal(got, every) {
				t.Errorf("carol's search with PRIV private finds %v, want every object, %v", got, every)
			}
			if got := agents(callerAlice); !slices.Equal(got, []string{"session-pub"}) {
				t.Errorf("alice's agents with PRIV private = %v, want PUB's session alone", got)
			}
			if got := agents(callerCarol); !slices.Equal(got, []string{"session-priv", "session-pub"}) {
				t.Errorf("carol's agents with PRIV private = %v, want both sessions", got)
			}
		})
	}
}
