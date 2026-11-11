package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// recordGoldenIssue is the issue the record golden is written from: the richest of the fixture,
// carrying an architect, its workers, its slot, its design gate, its pull request, the gated
// steps and its tree's memory draw.
const recordGoldenIssue = "LEGION-208"

// recordAsked is GET /legion/v1/issues/{key}/record on handler, asked from remote with no
// credential of any kind, as the state route is asked.
func recordAsked(t *testing.T, handler http.Handler, remote, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/issues/"+key+"/record", nil)
	request.RemoteAddr = remote
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// recordOf is the record route's answer for key, which must be a 200, with the newline the
// writer ends every body with left off (writeJSON): that newline is no part of the record.
func recordOf(t *testing.T, handler http.Handler, remote, key string) []byte {
	t.Helper()
	recorder := recordAsked(t, handler, remote, key)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the record of %s = %d, want 200; body %s", key, recorder.Code, recorder.Body)
	}
	return []byte(strings.TrimSpace(recorder.Body.String()))
}

// issuesOf is each issue's record inside a whole-project body, as the bytes the route wrote for
// it, so a comparison with the per-issue route reads the two answers and not two decodings.
func issuesOf(t *testing.T, state []byte) map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Issues map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(state, &body); err != nil {
		t.Fatalf("decode the state %s: %v", state, err)
	}
	if len(body.Issues) == 0 {
		t.Fatalf("the state %s records no issue; this proves nothing", state)
	}
	return body.Issues
}

// The record route answers the very value the whole-project body holds for that issue, field for
// field, for every issue of the fixture: the two bodies are the same `api.Issue` of the same
// snapshot, so their bytes are equal, and this compares the marshalled JSON of the two rather
// than a list of fields someone has to keep. That is what the route is built from the project's
// own state for: `waitingLine` and `slot` come from the project's admission, `check` from its
// open production checks, `gatedSteps` from the deployment's steps and the three memory fields
// from the tree's draw, so a cheaper query of one issue's row would answer a record missing six
// things, and this case is what stops a later change from writing one.
//
// Both answers are read at this daemon's own contract. The record answer arrived at 60
// (api.Answers), so no caller below it calls this route at all, and the equality is held where
// both answers are the ones this contract writes.
func TestTheRecordEqualsTheIssuesRecordInTheWholeProjectBody(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(DaemonAPIVersion))
	issues := issuesOf(t, stateAsked(t, handler, statePodAddr+":41000"))

	for key, inState := range issues {
		t.Run(key, func(t *testing.T) {
			record := recordOf(t, handler, statePodAddr+":41000", key)

			if string(record) != string(inState) {
				t.Errorf("the record of %s = %s,\nwant the record the whole-project body holds %s", key, record, inState)
			}
		})
	}
}

// The route answers any issue the caller names, not the caller's own alone: it authenticates
// nobody, and the pod it places the caller in decides the shape of the answer and nothing about
// which issue may be read. The caller here is the pod running LEGION-208's workers, and it reads
// LEGION-210, an issue of another tree whose claims run elsewhere.
func TestTheRecordRouteAnswersAnIssueThatIsNotTheCallersOwn(t *testing.T) {
	const other = "LEGION-210"
	handler := stateServer(t, populatedState(t), podClaims(DaemonAPIVersion))
	issues := issuesOf(t, stateAsked(t, handler, statePodAddr+":41000"))
	if _, recorded := issues[other]; !recorded {
		t.Fatalf("the fixture records no %s; this proves nothing", other)
	}

	record := recordOf(t, handler, statePodAddr+":41000", other)

	if string(record) != string(issues[other]) {
		t.Errorf("the record of %s = %s, want the record the whole-project body holds %s", other, record, issues[other])
	}
}

// An issue the daemon holds no record for is a 404 carrying RECORD_NOT_FOUND, a code no other
// refusal of this daemon uses, and the words the plugin's own `read_record` answers for an
// unknown issue today (`recordFrom` in `packages/pi-shared/src/legion-phase-ops.ts`: "The daemon
// has no record for <issue>"). A role that asks for the wrong key reads the sentence it has
// always read, whichever side found the record missing.
func TestTheRecordRouteRefusesAnIssueItHoldsNoRecordFor(t *testing.T) {
	const unknown = "LEGION-999"
	handler := stateServer(t, populatedState(t), podClaims(DaemonAPIVersion))
	if _, recorded := issuesOf(t, stateAsked(t, handler, statePodAddr+":41000"))[unknown]; recorded {
		t.Fatalf("the fixture records %s; this proves nothing", unknown)
	}

	recorder := recordAsked(t, handler, statePodAddr+":41000", unknown)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("the record of an unrecorded issue = %d, want 404; body %s", recorder.Code, recorder.Body)
	}
	var refusal Failure
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body, err)
	}
	if refusal.Code != recordNotFound {
		t.Errorf("the refusal's code = %q, want %q", refusal.Code, recordNotFound)
	}
	if want := "The daemon has no record for " + unknown; refusal.Error != want {
		t.Errorf("the refusal = %q, want the words read_record answers today, %q", refusal.Error, want)
	}
}

// The route is reached with no credential from an address no pod of the project claims — the
// operator's own command, a pane under the tmux runtime, anything else that reaches the API port
// — and is answered the body this contract writes, as the state route answers such a caller.
func TestTheRecordRouteAnswersACallerNoPodClaims(t *testing.T) {
	handler := stateServer(t, populatedState(t), podClaims(ServingFloor))

	record := recordAsked(t, handler, "192.0.2.1:41000", recordGoldenIssue)

	if record.Code != http.StatusOK {
		t.Fatalf("the record = %d, want 200; body %s", record.Code, record.Body)
	}
	written, err := json.Marshal(populatedState(t).Issues[recordGoldenIssue])
	if err != nil {
		t.Fatalf("marshal the fixture's record: %v", err)
	}
	// The writer ends every body with a newline (writeJSON), which is no part of the record.
	if got := strings.TrimSpace(record.Body.String()); got != string(written) {
		t.Errorf("the record = %s,\nwant the record this contract writes %s", got, written)
	}
}

// The health body names the daemon's own project, so a caller that reached a daemon reads which
// project it reached without asking for the whole state for one word.
func TestHealthNamesTheDaemonsProject(t *testing.T) {
	server := NewServer("127.0.0.1", 8437, Options{
		Steps: config.DesignGateRootIssues.Steps(),
		State: fakeSource{}, StateTransactions: &fakeStateTransactions{tx: &fakeTx{}},
		Project: testProject,
	})
	recorder := httptest.NewRecorder()

	server.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var health Health
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode the health body %s: %v", recorder.Body, err)
	}
	if health.Status != "ok" || health.Project != testProject {
		t.Errorf("the health body = %+v, want the daemon's own project %q and a status of ok", health, testProject)
	}
}
