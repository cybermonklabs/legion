// The client's write of a step record (PutStep) against Dispatch's step route, as the daemon's
// outbox sends it.
package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A step record is a PUT on the record's own id under its issue, as the daemon's session of the
// issue's project, the one session Dispatch's route takes a record from. Dispatch writes each field
// once and keeps what a write leaves out, so the body names every identity field, an empty type
// and a place of 0 among them, and leaves out each field the daemon does not know yet.
func TestPutStepWritesTheRecordUnderTheDaemonSession(t *testing.T) {
	const id = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"
	started := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	body := putStep(t, id, StepWrite{Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
		Agent: "legion-leg-leg-1-implementer", Generation: 2, StartedAt: &started})
	for name, want := range map[string]string{
		"task": `"default"`, "step": "0", "kind": `"retro"`, "role": `"implementer"`, "type": `""`,
		"agent": `"legion-leg-leg-1-implementer"`, "generation": "2", "started_at": `"2026-11-16T23:00:00Z"`,
		"actor": `{"kind":"session","id":"legion-daemon:LEG"}`,
	} {
		if got := string(body[name]); got != want {
			t.Errorf("the body writes %s as %q, want %s", name, got, want)
		}
	}
	for _, name := range []string{"id", "session", "model", "ended_at", "result", "returned_to", "report"} {
		if raw, ok := body[name]; ok {
			t.Errorf("the body writes %s as %s, want it left out", name, raw)
		}
	}
}

// The end of a merge that moved back to the retro names each field it knows under the name
// Dispatch's route reads, the report of the completion that ended it among them: the route ignores
// a member it does not declare, so a field under another name would be lost with no refusal. Its
// returned_to is 0, the retro's place, written as a value, where a write that returns nowhere
// leaves it out (TestPutStepWritesTheRecordUnderTheDaemonSession), so Dispatch can tell the two
// apart.
func TestPutStepWritesAnEndUnderTheNamesTheRouteReads(t *testing.T) {
	const id = "1c2d3e4f-5a6b-5c7d-8e9f-0a1b2c3d4e5f"
	ended := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	session, model, result, report, retro := "ses-merger", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", 0
	body := putStep(t, id, StepWrite{Task: "default", Step: 0, Kind: "merge", Role: "merger", Type: "",
		Agent: "legion-leg-leg-1-merger", Generation: 1, Session: &session, Model: &model, EndedAt: &ended,
		Result: &result, ReturnedTo: &retro, Report: &report})
	for name, want := range map[string]string{
		"kind": `"merge"`, "step": "0", "session": `"ses-merger"`, "model": `"anthropic/claude-opus-5"`,
		"ended_at": `"2026-11-17T02:00:00Z"`, "result": `"failed"`, "returned_to": "0",
		"report": `"6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"`,
	} {
		if got, ok := body[name]; !ok || string(got) != want {
			t.Errorf("the body writes %s as %q (written: %t), want %s", name, got, ok, want)
		}
	}
}

// putStep writes the record id of LEG-1 through the client to Dispatch's step route as a test plays
// it: a PUT on that record under the daemon's bearer, answered with the record. It returns the body
// the route read, once the route has answered.
func putStep(t *testing.T, id string, write StepWrite) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/v1/issues/LEG-1/steps/"+id {
			t.Fatalf("request = %s %s, want PUT /api/v1/issues/LEG-1/steps/%s", r.Method, r.URL.EscapedPath(), id)
		}
		assertBearer(t, r)
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]any{"id": id, "issue": "LEG-1"})
	}))
	defer server.Close()
	if err := New(server.URL, dispatchToken).PutStep(context.Background(), "LEG-1", id, write); err != nil {
		t.Fatalf("write the step record: %v", err)
	}
	return body
}
