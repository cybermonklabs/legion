package api

import (
	"bytes"
	"cmp"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The step records of an issue. The Legion daemon writes each run of each step twice, at its
// start and at its end, under one id (PUT /api/v1/issues/{key}/steps/{id}), and a person reads
// every run of the issue (GET /api/v1/issues/{key}/steps). The rows below drive both routes over
// the credentials a daemon and a person present, and never over the store itself.

// The start and the end of the runs the rows below write.
var (
	stepStart = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	stepEnd   = stepStart.Add(25 * time.Minute)
)

// stepIdentity is what every write of the implementing step of an issue's first run names, its
// start's and its end's alike: the task, the place, the kind, the role, the type and the run.
func stepIdentity(actor map[string]any) map[string]any {
	return map[string]any{
		"actor": actor, "task": "default", "step": 2, "kind": "step", "role": "implementer",
		"type": "implementation", "generation": 1,
	}
}

// implementerStart is the start of that step as the daemon writes it: the identity, the agent
// and the moment the role was entered.
func implementerStart(actor map[string]any) map[string]any {
	body := stepIdentity(actor)
	body["agent"] = "legion-leg-leg-1-implementer"
	body["started_at"] = stepStart
	return body
}

// implementerEnd is that step's end: the identity, how the run ended and when, and the model that
// answered it.
func implementerEnd(actor map[string]any, result string, at time.Time) map[string]any {
	body := stepIdentity(actor)
	body["result"] = result
	body["ended_at"] = at
	body["model"] = "anthropic/claude-opus-5"
	return body
}

// withStepFields is the step body with the fields of more set over it, and body itself unchanged.
func withStepFields(body map[string]any, more map[string]any) map[string]any {
	merged := maps.Clone(body)
	maps.Copy(merged, more)
	return merged
}

// putStep writes body as the step record id of issue over the credential the deployment
// configured for its daemon. That credential proves no session, so the body names the writer.
func putStep(t *testing.T, handler http.Handler, issue, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+id, body)
}

// putStepOK is putStep for a write the row expects to land, answering the record it answered.
func putStepOK(t *testing.T, handler http.Handler, issue, id string, body any) model.StepRecord {
	t.Helper()
	response := putStep(t, handler, issue, id, body)
	if response.Code != http.StatusOK {
		t.Fatalf("write step %s of %s: status=%d body=%s, want 200", id, issue, response.Code, response.Body.String())
	}
	return decodeBody[model.StepRecord](t, response)
}

// readSteps reads the steps of issue as a person reads them.
func readSteps(t *testing.T, handler http.Handler, issue string) model.IssueSteps {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the steps of %s: status=%d body=%s, want 200", issue, response.Code, response.Body.String())
	}
	return decodeBody[model.IssueSteps](t, response)
}

// stepsOf is every record the read of issue lists, task after task.
func stepsOf(t *testing.T, handler http.Handler, issue string) []model.StepRecord {
	t.Helper()
	var steps []model.StepRecord
	for _, task := range readSteps(t, handler, issue).Tasks {
		steps = append(steps, task.Steps...)
	}
	return steps
}

// stepOf is the run id as the read of issue lists it; a read that does not list it fails the test.
func stepOf(t *testing.T, handler http.Handler, issue, id string) model.StepRecord {
	t.Helper()
	steps := stepsOf(t, handler, issue)
	index := slices.IndexFunc(steps, func(step model.StepRecord) bool { return step.ID == id })
	if index < 0 {
		t.Fatalf("the read of %s does not list the run %s", issue, id)
	}
	return steps[index]
}

// A start and then an end of one run, as the daemon writes them, read back by a person: one task,
// one record holding the start's fields and the end's, and its attempt. The end's answer is the
// record as the read lists it.
func TestTheDaemonWritesAStepAndAReaderReadsIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	id := uuid.NewString()

	started := putStepOK(t, handler, issue, id, implementerStart(daemonSessionOf("LEG")))
	if started.StartedAt == nil || !started.StartedAt.Equal(stepStart) || started.Result != "" || started.EndedAt != nil {
		t.Fatalf("the start's answer is %s, want the run started at %s and not ended", asJSON(t, started), stepStart)
	}
	ended := putStepOK(t, handler, issue, id, implementerEnd(daemonSessionOf("LEG"), "passed", stepEnd))

	read := readSteps(t, handler, issue)
	if read.Issue != issue || len(read.Tasks) != 1 || read.Tasks[0].Task != "default" || len(read.Tasks[0].Steps) != 1 {
		t.Fatalf("the read of %s is %+v, want the one task default with one step", issue, read)
	}
	want := model.StepRecord{
		ID: id, Issue: issue, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Model: "anthropic/claude-opus-5",
		Generation: 1, Attempt: 1, StartedAt: &stepStart, EndedAt: &stepEnd, Result: "passed",
	}
	if got := asJSON(t, read.Tasks[0].Steps[0]); got != asJSON(t, want) {
		t.Fatalf("the read lists\n%s\nwant\n%s", got, asJSON(t, want))
	}
	if got := asJSON(t, ended); got != asJSON(t, want) {
		t.Fatalf("the end's answer is\n%s\nwant the record as the read lists it\n%s", got, asJSON(t, want))
	}
	// A record whose run never reported answers no report and no step it returned to, as null.
	raw := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice").Body.String()
	for _, absent := range []string{`"returned_to":null`, `"report":null`} {
		if !strings.Contains(raw, absent) {
			t.Fatalf("the read %s does not carry %s", raw, absent)
		}
	}
}

// A run's session, its report and its generation reach the store from the body and come back from
// it as written. The start names no session, as the daemon's start does, and the end names the
// session with the model, as one pair; a later end that names another session leaves it. The
// write's answers and the read carry that pair, the report in its canonical spelling and
// generation 2, beside its attempt 1 and the place it returned to.
func TestARunsSessionReportAndGenerationComeBackAsWritten(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	id, report := uuid.NewString(), uuid.NewString()
	identity := withStepFields(stepIdentity(daemonSessionOf("LEG")), map[string]any{"generation": 2})
	started := putStepOK(t, handler, issue, id, withStepFields(identity, map[string]any{
		"agent": "legion-leg-leg-1-implementer", "started_at": stepStart,
	}))
	if started.Session != "" || started.Model != "" {
		t.Fatalf("the start answers %s, want no session and no model yet", asJSON(t, started))
	}
	ended := putStepOK(t, handler, issue, id, withStepFields(identity, map[string]any{
		"result": "failed", "ended_at": stepEnd, "model": "anthropic/claude-opus-5",
		"session": "ses_implementer_run", "report": strings.ToUpper(report), "returned_to": 1,
	}))

	returned := 1
	want := model.StepRecord{
		ID: id, Issue: issue, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Session: "ses_implementer_run",
		Model: "anthropic/claude-opus-5", Generation: 2, Attempt: 1, StartedAt: &stepStart,
		EndedAt: &stepEnd, Result: "failed", ReturnedTo: &returned, Report: &report,
	}
	if got := asJSON(t, ended); got != asJSON(t, want) {
		t.Fatalf("the end's answer is\n%s\nwant\n%s", got, asJSON(t, want))
	}
	again := putStepOK(t, handler, issue, id, withStepFields(identity, map[string]any{
		"result": "failed", "ended_at": stepEnd, "session": "ses_a_later_session",
	}))
	if got := asJSON(t, again); got != asJSON(t, want) {
		t.Fatalf("the repeated end's answer is\n%s\nwant the first end's session\n%s", got, asJSON(t, want))
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || asJSON(t, steps[0]) != asJSON(t, want) {
		t.Fatalf("the read lists %+v, want the one record\n%s", steps, asJSON(t, want))
	}
}

// A second run of one step answers attempt 2, on its start and on its end, and the end's answer is
// the record as the read lists it; the first run still reads attempt 1.
func TestASecondRunOfAStepAnswersAttemptTwo(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	first, second := uuid.NewString(), uuid.NewString()
	putStepOK(t, handler, issue, first, implementerStart(daemon))
	putStepOK(t, handler, issue, first, implementerEnd(daemon, "stopped", stepEnd))

	againStart, againEnd := stepEnd.Add(time.Minute), stepEnd.Add(time.Hour)
	started := putStepOK(t, handler, issue, second, withStepFields(implementerStart(daemon), map[string]any{"started_at": againStart}))
	if started.Attempt != 2 || started.Generation != 1 {
		t.Fatalf("the second run's start answers %s, want attempt 2 of generation 1", asJSON(t, started))
	}
	ended := putStepOK(t, handler, issue, second, implementerEnd(daemon, "passed", againEnd))
	want := model.StepRecord{
		ID: second, Issue: issue, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Model: "anthropic/claude-opus-5",
		Generation: 1, Attempt: 2, StartedAt: &againStart, EndedAt: &againEnd, Result: "passed",
	}
	if got := asJSON(t, ended); got != asJSON(t, want) {
		t.Fatalf("the second run's end answers\n%s\nwant\n%s", got, asJSON(t, want))
	}
	steps := stepsOf(t, handler, issue)
	if len(steps) != 2 || steps[0].ID != first || steps[0].Attempt != 1 || asJSON(t, steps[1]) != asJSON(t, want) {
		t.Fatalf("the read lists %+v, want the first run at attempt 1 and then\n%s", steps, asJSON(t, want))
	}
}

// The read answers each task once, in the order of its first step, with its steps in the order
// they ran.
func TestTheReadListsTasksInTheOrderOfTheirFirstStep(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	run := func(task string, place int, at time.Time) string {
		id := uuid.NewString()
		putStepOK(t, handler, issue, id, map[string]any{
			"actor": daemon, "task": task, "step": place, "kind": "step", "role": "implementer",
			"type": "implementation", "generation": 1, "started_at": at,
		})
		return id
	}
	firstDocs := run("docs", 1, stepStart)
	code := run("default", 1, stepStart.Add(time.Minute))
	secondDocs := run("docs", 2, stepStart.Add(2*time.Minute))

	read := readSteps(t, handler, issue)
	var got []string
	for _, task := range read.Tasks {
		for _, step := range task.Steps {
			got = append(got, task.Task+"/"+step.ID)
		}
	}
	want := []string{"docs/" + firstDocs, "docs/" + secondDocs, "default/" + code}
	if !slices.Equal(got, want) {
		t.Fatalf("the read lists %v, want %v", got, want)
	}
}

// Only the session of the issue's project's daemon writes a record: a person, the daemon of
// another project and an agent's session are each refused, and that daemon's write lands.
func TestAWriteThatNamesAnotherSessionIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	path := "/api/v1/issues/" + issue + "/steps/"

	person := dispatchRequest(t, handler, http.MethodPut, path+uuid.NewString(), implementerStart(nil), "alice")
	expectRefusal(t, "a person's write", person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	for _, session := range []string{legionDaemonSessionPrefix + "OTHER", "ses_agent"} {
		refused := putStep(t, handler, issue, uuid.NewString(), implementerStart(map[string]any{"kind": "session", "id": session}))
		expectRefusal(t, "a write as "+session, refused, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}

	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSessionOf("LEG")))
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != id {
		t.Fatalf("the daemon's write left %+v, want the one record %s", steps, id)
	}
}

// A closed issue takes the record of the merge that closed it, where a handoff, which a running
// role writes, is refused there.
func TestAClosedIssueTakesItsMergeRecord(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	closeIssue(t, handler, issue)

	assertRefusedOnClosedIssue(t, "a handoff", postHandoff(t, handler, issue, "implement", `{}`))
	id := uuid.NewString()
	merged := putStepOK(t, handler, issue, id, map[string]any{
		"actor": daemonSessionOf("LEG"), "task": "default", "step": 0, "kind": "merge", "role": "merger",
		"generation": 1, "result": "passed", "ended_at": stepEnd,
	})
	if merged.Kind != "merge" || merged.Step != 0 || merged.Agent != "" || merged.Result != "passed" || merged.Attempt != 1 {
		t.Fatalf("the merge record is %s, want a merge at step 0 with no agent, passed, attempt 1", asJSON(t, merged))
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != id {
		t.Fatalf("the closed issue's read lists %+v, want its merge record", steps)
	}
}

// A write to an issue Dispatch does not hold is 404 ISSUE_NOT_FOUND, a code of its own, which the
// daemon tells from the 404 of a Dispatch that does not serve the route: that answer carries no
// such code.
func TestAnUnknownIssueIsIssueNotFound(t *testing.T) {
	handler := newTestHandler(t)
	createTestIssue(t, handler, "LEG", "A run of steps")

	missing := putStep(t, handler, "LEG-999", uuid.NewString(), implementerStart(daemonSessionOf("LEG")))
	expectRefusal(t, "a write to LEG-999", missing, http.StatusNotFound, "ISSUE_NOT_FOUND", "LEG-999")
	unrouted := sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/LEG-999/no-such-records/"+uuid.NewString(),
		implementerStart(daemonSessionOf("LEG")))
	if unrouted.Code != http.StatusNotFound || strings.Contains(unrouted.Body.String(), "ISSUE_NOT_FOUND") {
		t.Fatalf("a write to a route no row serves: status=%d body=%s, want a 404 without ISSUE_NOT_FOUND", unrouted.Code, unrouted.Body.String())
	}
}

// The read of an issue Dispatch does not hold answers as the read of the issue itself does, 404
// NOT_FOUND, so a mistyped key never reads as an issue Legion never ran; and that answer is the
// handler's, not the router's "no route" of a Dispatch older than the route. An issue Dispatch
// holds that no step ran on still reads 200 and no task.
func TestAnUnknownIssueReadsAsTheIssueRead(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "An issue Legion never ran")

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/LEG-999", nil, "alice")
	expectRefusal(t, "the issue read of LEG-999", read, http.StatusNotFound, "NOT_FOUND")
	steps := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/LEG-999/steps", nil, "alice")
	if steps.Code != read.Code || strings.TrimSpace(steps.Body.String()) != strings.TrimSpace(read.Body.String()) {
		t.Fatalf("the steps of LEG-999: status=%d body=%s, want the issue read's status=%d body=%s",
			steps.Code, steps.Body.String(), read.Code, read.Body.String())
	}
	if refusal := decodeBody[refusalAnswer](t, steps); strings.HasPrefix(refusal.Error, "no route") {
		t.Fatalf("the steps of LEG-999 answer the router's refusal %q, want the handler's", refusal.Error)
	}

	held := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice")
	if want := `{"issue":"` + issue + `","tasks":[]}`; held.Code != http.StatusOK || strings.TrimSpace(held.Body.String()) != want {
		t.Fatalf("the steps of %s, which no step ran on: status=%d body=%s, want 200 %s", issue, held.Code,
			held.Body.String(), want)
	}
}

// A step write appends no event to its issue, whose activity a handoff moves.
func TestAStepWriteAppendsNoEvent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	before := issueEventTypes(t, handler, issue)

	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSessionOf("LEG")))
	putStepOK(t, handler, issue, id, implementerEnd(daemonSessionOf("LEG"), "failed", stepEnd))
	if after := issueEventTypes(t, handler, issue); !slices.Equal(before, after) {
		t.Fatalf("the issue's events went from %v to %v, want no event for a step write", before, after)
	}

	if recorded := postHandoff(t, handler, issue, "implement", `{}`); recorded.Code != http.StatusCreated {
		t.Fatalf("the handoff: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if after := issueEventTypes(t, handler, issue); len(after) != len(before)+1 || after[len(after)-1] != "handoff.recorded" {
		t.Fatalf("after a handoff the issue's events are %v, want %v and handoff.recorded: the read sees no event", after, before)
	}
}

// Each value outside its form is refused 400 with its field named, and a body Dispatch cannot
// decode is the package's own 400; none is a 500, and none writes a record.
func TestAStepWriteRefusesItsBadValues(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	start := implementerStart(daemon)
	without := func(field string) map[string]any {
		body := maps.Clone(start)
		delete(body, field)
		return body
	}

	for _, row := range []struct {
		name, id string
		body     map[string]any
		field    string
	}{
		{"a kind that is no kind", "", withStepFields(start, map[string]any{"kind": "phase"}), "kind"},
		{"a result that is no result", "", withStepFields(start, map[string]any{"result": "done", "ended_at": stepEnd}), "result"},
		{"an end with no result", "", withStepFields(start, map[string]any{"ended_at": stepEnd}), "ended_at"},
		{"a result with no end", "", withStepFields(start, map[string]any{"result": "passed"}), "result"},
		{"a step returned to by a run that passed", "", withStepFields(start, map[string]any{"result": "passed", "ended_at": stepEnd, "returned_to": 2}), "returned_to"},
		{"a retro returned to by a run that passed", "", withStepFields(start, map[string]any{"result": "passed", "ended_at": stepEnd, "returned_to": 0}), "returned_to"},
		{"a returned_to below 0", "", withStepFields(start, map[string]any{"result": "failed", "ended_at": stepEnd, "returned_to": -1}), "returned_to -1"},
		{"a step returned to by a write that names no result", "", withStepFields(start, map[string]any{"returned_to": 2}), `this write's result is ""`},
		{"a first write with no task", "", without("task"), "does not name its task:"},
		{"a first write with no kind", "", without("kind"), "does not name its kind:"},
		{"a first write with no step", "", without("step"), "does not name its step:"},
		{"a first write with no role", "", without("role"), "does not name its role:"},
		{"a first write with no generation", "", without("generation"), "does not name its generation:"},
		{"a generation that is no run", "", withStepFields(start, map[string]any{"generation": 0}), "generation"},
		{"a place below 0", "", withStepFields(without("kind"), map[string]any{"step": -1}), "0 or more"},
		{"a step of kind step at place 0", "", withStepFields(start, map[string]any{"step": 0}), "count from 1"},
		{"a retro at a step's place", "", withStepFields(start, map[string]any{"kind": "retro", "type": ""}), "its step is 0"},
		{"a merge at a step's place", "", withStepFields(start, map[string]any{"kind": "merge", "type": "", "role": "merger"}), "its step is 0"},
		{"a role that is no role", "", withStepFields(start, map[string]any{"role": "Implementer"}), "role"},
		{"a type that is no type", "", withStepFields(start, map[string]any{"type": "Review"}), "type"},
		{"a retro with a type", "", withStepFields(start, map[string]any{"kind": "retro", "step": 0}), "type"},
		{"a merge with a type", "", withStepFields(start, map[string]any{"kind": "merge", "step": 0, "role": "merger"}), "type"},
		{"a report that is no uuid", "", withStepFields(start, map[string]any{"result": "passed", "ended_at": stepEnd, "report": "plan-1"}), "report"},
		{"a task that is no task id", "", withStepFields(start, map[string]any{"task": "Default"}), "task"},
		{"an id that is no uuid", "not-a-uuid", start, "id"},
		{"a start before the UTC year range", "", withStepFields(start, map[string]any{"started_at": "0000-01-01T00:00:00+01:00"}), "started_at"},
		{"a start after the UTC year range", "", withStepFields(start, map[string]any{"started_at": "9999-12-31T23:59:59-01:00"}), "started_at"},
		{"an end before the UTC year range", "", withStepFields(start, map[string]any{"result": "failed", "ended_at": "0000-01-01T00:00:00+01:00"}), "ended_at"},
		{"an end after the UTC year range", "", withStepFields(start, map[string]any{"result": "failed", "ended_at": "9999-12-31T23:59:59-01:00"}), "ended_at"},
	} {
		t.Run(row.name, func(t *testing.T) {
			id := cmp.Or(row.id, uuid.NewString())
			expectRefusal(t, row.name, putStep(t, handler, issue, id, row.body), http.StatusBadRequest, codeStepInput, row.field)
		})
	}
	for name, raw := range map[string]string{
		"a body that is not JSON":   `{"task":`,
		"a step that is no number":  `{"actor":{"kind":"session","id":"legion-daemon:LEG"},"step":"two"}`,
		"a start that is no moment": `{"actor":{"kind":"session","id":"legion-daemon:LEG"},"started_at":"yesterday"}`,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+uuid.NewString(), bytes.NewReader([]byte(raw)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer agent-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			expectRefusal(t, name, response, http.StatusBadRequest, "INVALID_JSON")
		})
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}
}

// Normalizing an offset must keep a moment readable at either end of the JSON timestamp range.
func TestAStepMomentAtTheUTCYearBoundsIsReadable(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	for _, row := range []struct{ input, want string }{
		{"0000-01-01T01:00:00+01:00", "0000-01-01T00:00:00Z"},
		{"9999-12-31T22:59:59-01:00", "9999-12-31T23:59:59Z"},
	} {
		id := uuid.NewString()
		written := putStepOK(t, handler, issue, id, withStepFields(implementerStart(daemonSessionOf("LEG")), map[string]any{"started_at": row.input}))
		if written.StartedAt == nil || written.StartedAt.Format(time.RFC3339Nano) != row.want {
			t.Fatalf("the write of %s answers %s, want start %s", row.input, asJSON(t, written), row.want)
		}
		if step := stepOf(t, handler, issue, id); step.StartedAt == nil || step.StartedAt.Format(time.RFC3339Nano) != row.want {
			t.Fatalf("the read lists %s, want start %s", asJSON(t, step), row.want)
		}
	}
}

// The start and the end of one new run, sent at once, both land, and the record holds the fields
// of each: the second write waits for the first and reads what it wrote.
func TestTwoPutsOfOneStepAtOneTimeKeepBoth(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	start := implementerStart(daemon)
	end := implementerEnd(daemon, "passed", stepEnd)

	const rounds = 20
	ids := make([]string, rounds)
	for round := range rounds {
		ids[round] = uuid.NewString()
		release := make(chan struct{})
		responses := make([]*httptest.ResponseRecorder, 2)
		var sent sync.WaitGroup
		for index, body := range []map[string]any{start, end} {
			sent.Go(func() {
				<-release
				data, err := json.Marshal(body)
				if err != nil {
					panic(err)
				}
				request := httptest.NewRequest(http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+ids[round], bytes.NewReader(data))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer agent-token")
				responses[index] = httptest.NewRecorder()
				handler.ServeHTTP(responses[index], request)
			})
		}
		close(release)
		sent.Wait()
		for index, response := range responses {
			if response.Code != http.StatusOK {
				t.Fatalf("round %d, write %d: status=%d body=%s, want 200", round, index, response.Code, response.Body.String())
			}
		}
	}

	steps := stepsOf(t, handler, issue)
	byID := map[string]model.StepRecord{}
	for _, step := range steps {
		if _, twice := byID[step.ID]; twice {
			t.Fatalf("the read lists %s twice", step.ID)
		}
		byID[step.ID] = step
	}
	for round, id := range ids {
		step, listed := byID[id]
		if !listed || step.StartedAt == nil || !step.StartedAt.Equal(stepStart) || step.Agent == "" ||
			step.Result != "passed" || step.EndedAt == nil || !step.EndedAt.Equal(stepEnd) || step.Model == "" {
			t.Fatalf("round %d: the read lists %s, want the start's started_at and agent beside the end's result, ended_at and model", round, asJSON(t, step))
		}
	}
	if len(steps) != rounds {
		t.Fatalf("the read lists %d records, want one for each of the %d rounds", len(steps), rounds)
	}
}

// A second end of one run lands and changes nothing: the run keeps its first end.
func TestAnEndPutTwiceKeepsItsFirstEnd(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSessionOf("LEG")))
	putStepOK(t, handler, issue, id, implementerEnd(daemonSessionOf("LEG"), "stopped", stepEnd))

	again := putStepOK(t, handler, issue, id, implementerEnd(daemonSessionOf("LEG"), "passed", stepEnd.Add(time.Hour)))
	if again.Result != "stopped" || again.EndedAt == nil || !again.EndedAt.Equal(stepEnd) {
		t.Fatalf("the second end answers %s, want the first end, stopped at %s", asJSON(t, again), stepEnd)
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].Result != "stopped" || !steps[0].EndedAt.Equal(stepEnd) {
		t.Fatalf("the read lists %+v, want the one run ended stopped at %s", steps, stepEnd)
	}
}

// A backward move to the retro returns to place 0, not to no place. Both delivery orders and a
// later end keep that first return destination in the write's answer and in the issue's read.
func TestAStepReturnedToRetroKeepsPlaceZero(t *testing.T) {
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	for _, endFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(endFirst), func(t *testing.T) {
			id := uuid.NewString()
			identity := map[string]any{
				"actor": daemonSessionOf("LEG"), "task": "default", "step": 0, "kind": "merge",
				"role": "merger", "type": "", "agent": "legion-leg-leg-1-merger", "generation": 1,
			}
			start := withStepFields(identity, map[string]any{"started_at": stepStart})
			end := withStepFields(identity, map[string]any{"ended_at": stepEnd, "result": "failed", "returned_to": 0})
			if !endFirst {
				started := putStepOK(t, handler, issue, id, start)
				if started.ReturnedTo != nil {
					t.Fatalf("a start returned to %v, want no destination", started.ReturnedTo)
				}
			}
			ended := putStepOK(t, handler, issue, id, end)
			if ended.ReturnedTo == nil || *ended.ReturnedTo != 0 {
				t.Fatalf("the end answers %s, want returned_to 0, not null", asJSON(t, ended))
			}
			if endFirst {
				putStepOK(t, handler, issue, id, start)
			}
			again := putStepOK(t, handler, issue, id, withStepFields(end, map[string]any{"returned_to": 2}))
			if again.ReturnedTo == nil || *again.ReturnedTo != 0 {
				t.Fatalf("the later end answers %s, want the first return to place 0", asJSON(t, again))
			}
			if step := stepOf(t, handler, issue, id); step.ReturnedTo == nil || *step.ReturnedTo != 0 {
				t.Fatalf("the read lists %s, want returned_to 0, not null", asJSON(t, step))
			}
		})
	}
}

// An id the record of one issue holds is refused to a write that names another issue.
func TestAStepOfAnotherIssueIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issues := []string{
		createTestIssue(t, handler, "LEG", "A run of steps"),
		createAdvancingIssue(t, handler, map[string]any{"project": "LEG", "title": "Another run of steps"}, "alice").Key,
	}
	id := uuid.NewString()
	putStepOK(t, handler, issues[0], id, implementerStart(daemonSessionOf("LEG")))

	other := putStep(t, handler, issues[1], id, implementerEnd(daemonSessionOf("LEG"), "passed", stepEnd))
	expectRefusal(t, "the same id on "+issues[1], other, http.StatusConflict, codeStepOtherIssue, issues[0], issues[1])
	if steps := stepsOf(t, handler, issues[1]); len(steps) != 0 {
		t.Fatalf("%s lists %+v, want nothing", issues[1], steps)
	}
	if steps := stepsOf(t, handler, issues[0]); len(steps) != 1 || steps[0].Result != "" {
		t.Fatalf("%s lists %+v, want its one run, not ended", issues[0], steps)
	}
}

// A credential scoped to one project writes the records of that project's issues and no other's,
// though it proves the session of that project's daemon.
func TestAProjectCredentialWritesOnlyItsProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-daemon-token-of-one-project"
	answer := liveNamedToken(caller.jkt)
	answer.Session = legionDaemonSessionPrefix + brokerTestProject
	rig.broker.Says(token, answer)
	issueOf := func(project string) string {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
		issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": "Steps of " + project})
		if issue.Code != http.StatusCreated {
			t.Fatalf("create an issue in %s: status=%d body=%s", project, issue.Code, issue.Body.String())
		}
		return decodeBody[struct {
			Key string `json:"key"`
		}](t, issue).Key
	}
	own, other := issueOf(brokerTestProject), issueOf("OTH")

	refused := rig.serve(caller.presenting(t, http.MethodPut, "/api/v1/issues/"+other+"/steps/"+uuid.NewString(), token,
		implementerStart(daemonSessionOf("OTH"))))
	expectRefusal(t, "a write to "+other, refused, http.StatusForbidden, codeProjectScope, brokerTestProject, "OTH")
	answered := rig.serve(caller.presenting(t, http.MethodPut, "/api/v1/issues/"+own+"/steps/"+uuid.NewString(), token,
		implementerStart(daemonSessionOf(brokerTestProject))))
	if answered.Code != http.StatusOK {
		t.Fatalf("the same write to %s: status=%d body=%s, want 200", own, answered.Code, answered.Body.String())
	}
}
