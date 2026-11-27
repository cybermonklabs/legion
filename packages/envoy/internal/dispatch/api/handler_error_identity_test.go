package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// handlerFailedLine is the message of the ERROR line writeHandlerError writes for a failure it
// cannot name, written out here rather than taken from the code under test.
const handlerFailedLine = "dispatch: API handler failed"

// testRowRefusal is the message refuseInserts' trigger raises, which the handler's line carries.
const testRowRefusal = "the test database refuses this row"

// refuseInserts makes the database refuse every insert into table for which when holds (every
// insert when it is empty), raising testRowRefusal, as a step whose write the database refused.
func refuseInserts(t *testing.T, database *store.Store, table, when string) {
	t.Helper()
	condition := ""
	if when != "" {
		condition = " when (" + when + ")"
	}
	for _, statement := range []string{
		`create function refuse_test_row() returns trigger language plpgsql as $$
		begin raise exception '` + testRowRefusal + `'; end $$`,
		`create trigger refuse_test_row before insert on ` + table + `
		  for each row` + condition + ` execute function refuse_test_row()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the database refuse %s's rows: %v", table, err)
		}
	}
}

// A request that fails in a step keyed by its caller is answered 500 INTERNAL, and the ERROR line
// writeHandlerError writes for it names the step and the database's refusal and never the caller:
// no person's email and no session id a credential proved, each of which a request header carries
// (CodeQL go/clear-text-logging). The steps are recording the person a header names
// (store.PgPeopleStore.Record), reading the session generation a cookie's person holds
// (store.PgSessionStore.CurrentSessionGeneration), and following an ask as the session a
// broker-issued token proved, both on the follow route (asks.Follow) and as the follow an ask that
// session opens queues (asks.QueueFollowAuthor).
func TestAStepKeyedByItsCallerLogsItsFailureWithoutTheCaller(t *testing.T) {
	for _, step := range []struct {
		name string
		// fail sends the request whose step fails, answering the response, the caller the line
		// must not name and the database's refusal it must carry.
		fail func(t *testing.T) (response *httptest.ResponseRecorder, caller, refusal string)
	}{
		{"recording the person a header names", func(t *testing.T) (*httptest.ResponseRecorder, string, string) {
			handler, database := newTestHandlerWithStore(t)
			const person = "carol@example.com"
			refuseInserts(t, database, "people", "new.email = '"+person+"'")
			return dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, person), person, testRowRefusal
		}},
		{"reading a cookie's session generation", func(t *testing.T) (*httptest.ResponseRecorder, string, string) {
			database := storetest.Open(t)
			_, _, _, request, _ := newCookieServerOn(t, database, 0)
			if _, err := database.Pool.Exec(context.Background(), `alter table user_sessions rename column generation to generation_moved`); err != nil {
				t.Fatalf("make the session generation unreadable: %v", err)
			}
			return request(http.MethodGet, "/api/v1/whoami", nil), "alice@d.example", "SQLSTATE 42703"
		}},
		{"following an ask as a broker-proven session", func(t *testing.T) (*httptest.ResponseRecorder, string, string) {
			rig := newBrokerTokenRig(t)
			caller := newBrokerCaller(t)
			const token = "a-token-whose-session-follows"
			rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
			issue := createInteractionIssue(t, rig.handler, "TEST", "Followers", "A spec")
			askID := openAskAs(t, rig.handler, issue.Key, "s1", "Ship it?")
			refuseInserts(t, rig.database, "ask_followers", "")
			path := "/api/v1/asks/" + askID + "/followers/" + brokerTestSession
			return rig.serve(caller.presenting(t, http.MethodPut, path, token, nil)), brokerTestSession, testRowRefusal
		}},
		{"opening an ask as a broker-proven session", func(t *testing.T) (*httptest.ResponseRecorder, string, string) {
			rig := newBrokerTokenRig(t)
			caller := newBrokerCaller(t)
			const token = "a-token-whose-session-asks"
			rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
			issue := createInteractionIssue(t, rig.handler, "TEST", "Asks", "A spec")
			refuseInserts(t, rig.database, "ask_followers", "")
			return rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", token, map[string]any{
				"question": "Ship it?",
				"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
			})), brokerTestSession, testRowRefusal
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			logged := captureLog(t)
			response, caller, refusal := step.fail(t)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"INTERNAL"`) {
				t.Fatalf("status=%d body=%s, want 500 INTERNAL", response.Code, response.Body.String())
			}
			lines := logged.Lines(handlerFailedLine)
			if !strings.Contains(lines, refusal) {
				t.Fatalf("no %q line carries the database's refusal %q; the lines:\n%s", handlerFailedLine, refusal, lines)
			}
			if strings.Contains(lines, caller) {
				t.Fatalf("the %q line names the caller %q:\n%s", handlerFailedLine, caller, lines)
			}
			t.Logf("the %q line: %s", handlerFailedLine, lines)
		})
	}
}
