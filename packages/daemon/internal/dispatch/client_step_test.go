// The client's writes of a step record, of a retro's rating, of the variant of a record's issue,
// of a run's commits and of a plan (PutStep, PutStepRating, PutStepExperiment, PutStepCommits,
// PutPlan) against Dispatch's step, rating, experiment, commits and plan routes, as the daemon's
// outbox sends them.
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
	for _, name := range []string{"id", "session", "model", "ended_at", "result", "returned_to", "report", "rating", "rating_reason"} {
		if raw, ok := body[name]; ok {
			t.Errorf("the body writes %s as %s, want it left out", name, raw)
		}
	}
}

// The rating a retro gave the plan goes to the record's rating route, PUT
// /api/v1/issues/{key}/steps/{id}/rating, never to the step route: Dispatch ignores a body member
// it does not declare, so a rating sent to the step route would be answered 200 and dropped. The
// body is the step write's, with the identity of the retro's run, which makes the record where its
// start and end have not landed yet, plus the rating and its reason.
func TestPutStepRatingWritesTheRatingOnTheRatingRoute(t *testing.T) {
	const id = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"
	write := StepWrite{Task: "default", Step: 0, Kind: "retro", Role: "implementer", Type: "",
		Agent: "legion-leg-leg-1-implementer", Generation: 2}
	body := putRecord(t, "/api/v1/issues/LEG-1/steps/"+id+"/rating", id, func(client *HTTPClient) error {
		return client.PutStepRating(context.Background(), "LEG-1", id, write, PlanRating{Rating: 4, Reason: "The plan put the tests after the review."})
	})
	for name, want := range map[string]string{
		"task": `"default"`, "step": "0", "kind": `"retro"`, "role": `"implementer"`, "type": `""`,
		"agent": `"legion-leg-leg-1-implementer"`, "generation": "2", "rating": "4",
		"rating_reason": `"The plan put the tests after the review."`, "actor": `{"kind":"session","id":"legion-daemon:LEG"}`,
	} {
		if got := string(body[name]); got != want {
			t.Errorf("the body writes %s as %q, want %s", name, got, want)
		}
	}
	for _, name := range []string{"id", "session", "model", "started_at", "ended_at", "result", "returned_to", "report"} {
		if raw, ok := body[name]; ok {
			t.Errorf("the body writes %s as %s, want it left out", name, raw)
		}
	}
}

// A run's commits and the check a fix's run is of go to the record's commits route, PUT
// /api/v1/issues/{key}/steps/{id}/commits, never to the step route, which ignores a member it
// does not declare and would answer 200 and drop them. The body is the run's identity, which makes
// the record where its start and end have not landed yet, and the commits under the names the
// route reads, and no other field of the record's write: the start and the end are the step
// route's. A commit the run does not name is left out, since Dispatch writes each once.
func TestPutStepCommitsWritesTheCommitsOnTheCommitsRoute(t *testing.T) {
	t.Parallel()
	const id = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"
	started, ended := time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC)
	session, model, result, report, back := "ses-implementer", "anthropic/claude-opus-5", "passed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", 1
	write := StepWrite{Task: "a", Step: 1, Kind: "step", Role: "implementer", Type: "implementation",
		Agent: "legion-leg-leg-1-implementer", Generation: 2, Session: &session, Model: &model, StartedAt: &started,
		EndedAt: &ended, Result: &result, ReturnedTo: &back, Report: &report}
	for _, tc := range []struct {
		name    string
		commits StepCommits
		want    map[string]string
		absent  []string
	}{
		{name: "the end of a fix", commits: StepCommits{StartCommit: "1111111111111111111111111111111111111111",
			EndCommit: "2222222222222222222222222222222222222222", FixCheck: "k-ci"},
			want: map[string]string{"start_commit": `"1111111111111111111111111111111111111111"`,
				"end_commit": `"2222222222222222222222222222222222222222"`, "fix_check": `"k-ci"`}},
		{name: "a start that names its commit alone", commits: StepCommits{StartCommit: "1111111111111111111111111111111111111111"},
			want: map[string]string{"start_commit": `"1111111111111111111111111111111111111111"`}, absent: []string{"end_commit", "fix_check"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := putRecord(t, "/api/v1/issues/LEG-1/steps/"+id+"/commits", id, func(client *HTTPClient) error {
				return client.PutStepCommits(context.Background(), "LEG-1", id, write, tc.commits)
			})
			want := map[string]string{
				"task": `"a"`, "step": "1", "kind": `"step"`, "role": `"implementer"`, "type": `"implementation"`,
				"agent": `"legion-leg-leg-1-implementer"`, "generation": "2", "actor": `{"kind":"session","id":"legion-daemon:LEG"}`,
			}
			for name, value := range tc.want {
				want[name] = value
			}
			for name, value := range want {
				if got := string(body[name]); got != value {
					t.Errorf("the body writes %s as %q, want %s", name, got, value)
				}
			}
			for _, name := range append([]string{"id", "session", "model", "started_at", "ended_at", "result", "returned_to",
				"report", "rating", "rating_reason"}, tc.absent...) {
				if raw, ok := body[name]; ok {
					t.Errorf("the body writes %s as %s, want it left out", name, raw)
				}
			}
		})
	}
}

// The experiment and the variant of a record's issue go to the record's experiment route, PUT
// /api/v1/issues/{key}/steps/{id}/experiment, never to the step route or the rating route:
// Dispatch ignores a body member a route does not declare, so a pair sent to either would be
// answered 200 and dropped by a Dispatch from before the experiment route, which answers that
// route its router's NOT_FOUND instead. The body is the run's identity and the pair alone: the
// write handed to the client carries the start's session and moment, which are the record's own
// write's (the outbox sends that one first), and none of them reaches this route.
func TestPutStepExperimentWritesTheVariantOnItsOwnRoute(t *testing.T) {
	const id = "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b"
	started := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	session, model, result, report, returnedTo := "ses-implementer", "anthropic/claude-opus-5", "failed", "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", 1
	write := StepWrite{Task: "default", Step: 2, Kind: "step", Role: "implementer", Type: "implementation",
		Agent: "legion-leg-leg-1-implementer", Generation: 1, Session: &session, Model: &model, StartedAt: &started,
		EndedAt: &started, Result: &result, ReturnedTo: &returnedTo, Report: &report}
	body := putRecord(t, "/api/v1/issues/LEG-1/steps/"+id+"/experiment", id, func(client *HTTPClient) error {
		return client.PutStepExperiment(context.Background(), "LEG-1", id, write, StepExperiment{Experiment: "planning", Variant: "b"})
	})
	for name, want := range map[string]string{
		"task": `"default"`, "step": "2", "kind": `"step"`, "role": `"implementer"`, "type": `"implementation"`,
		"agent": `"legion-leg-leg-1-implementer"`, "generation": "1", "experiment": `"planning"`, "variant": `"b"`,
		"actor": `{"kind":"session","id":"legion-daemon:LEG"}`,
	} {
		if got := string(body[name]); got != want {
			t.Errorf("the body writes %s as %q, want %s", name, got, want)
		}
	}
	for _, name := range []string{"id", "session", "model", "started_at", "ended_at", "result", "returned_to", "report", "rating", "rating_reason"} {
		if raw, ok := body[name]; ok {
			t.Errorf("the body writes %s as %s, want it left out", name, raw)
		}
	}
}

// The plan of one run of an issue is a PUT on the plan's own id under its issue, as the daemon's
// session of the issue's project, the one session Dispatch's route takes a plan from. The body
// names the run, the source and every task with its steps in order; a plan of the source default
// names no document and no version, which Dispatch refuses on such a plan, so the body leaves both
// out, and a plan of the source owner names both, under the names Dispatch's route reads. The
// daemon writes no owner's plan yet (the workflow's defaultPlan is its one plan), so that case
// holds the body of each field such a plan carries.
func TestPutPlanWritesThePlanUnderTheDaemonSession(t *testing.T) {
	const id = "5d6e7f80-91a2-5b3c-8d4e-5f6a7b8c9d0e"
	for _, tc := range []struct {
		name    string
		plan    PlanWrite
		present map[string]string
		absent  []string
	}{
		{
			name: "the default plan",
			plan: PlanWrite{Generation: 2, Source: "default",
				Tasks: []PlanTask{{ID: "default", Steps: []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}}}},
			present: map[string]string{"generation": "2", "source": `"default"`,
				"tasks": `[{"id":"default","steps":["planner","implementer","tester","reviewer","acceptance:owner"]}]`},
			absent: []string{"id", "artifact_id", "version"},
		},
		{
			name: "an owner's plan",
			plan: PlanWrite{Generation: 3, Source: "owner", PlanDocument: &PlanDocument{ArtifactID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 4},
				Tasks: []PlanTask{{ID: "ui", Steps: []string{"tester", "implementer:ui"}}}},
			present: map[string]string{"generation": "3", "source": `"owner"`, "artifact_id": `"6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"`,
				"version": "4", "tasks": `[{"id":"ui","steps":["tester","implementer:ui"]}]`},
			absent: []string{"id"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := putRecord(t, "/api/v1/issues/LEG-1/plans/"+id, id, func(client *HTTPClient) error {
				return client.PutPlan(context.Background(), "LEG-1", id, tc.plan)
			})
			tc.present["actor"] = `{"kind":"session","id":"legion-daemon:LEG"}`
			for name, want := range tc.present {
				if got := string(body[name]); got != want {
					t.Errorf("the body writes %s as %q, want %s", name, got, want)
				}
			}
			for _, name := range tc.absent {
				if raw, ok := body[name]; ok {
					t.Errorf("the body writes %s as %s, want it left out", name, raw)
				}
			}
		})
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
// it. It returns the body that route read.
func putStep(t *testing.T, id string, write StepWrite) map[string]json.RawMessage {
	t.Helper()
	return putRecord(t, "/api/v1/issues/LEG-1/steps/"+id, id, func(client *HTTPClient) error {
		return client.PutStep(context.Background(), "LEG-1", id, write)
	})
}

// putRecord is Dispatch's stand-in for one write of a record of LEG-1: it takes the PUT call makes
// on path under the daemon's bearer, answers it with the record id, and returns the body it read.
// A call to any other route, or with any other method, stops the test, so a client that writes a
// record to the wrong route fails by name rather than by the body it left behind.
func putRecord(t *testing.T, path, id string, call func(client *HTTPClient) error) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.EscapedPath() != path {
			t.Errorf("request = %s %s, want PUT %s", r.Method, r.URL.EscapedPath(), path)
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		assertBearer(t, r)
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]any{"id": id, "issue": "LEG-1"})
	}))
	defer server.Close()
	if err := call(New(server.URL, BearerTransport(dispatchToken, nil))); err != nil {
		t.Fatalf("write the record of LEG-1 to %s: %v", path, err)
	}
	return body
}
