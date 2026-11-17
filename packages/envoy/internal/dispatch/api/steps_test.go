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

// daemonSession is the writer the Legion daemon of project names on every step write.
func daemonSession(project string) map[string]any {
	return map[string]any{"kind": "session", "id": legionDaemonSessionPrefix + project}
}

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

// with is body with the fields of more set over it, and body itself unchanged.
func with(body map[string]any, more map[string]any) map[string]any {
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

// stepIssues makes project and count issues in it, as a person does, and answers their keys.
func stepIssues(t *testing.T, handler http.Handler, project string, count int) []string {
	t.Helper()
	createTestProject(t, handler, project)
	keys := make([]string, count)
	for index := range keys {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "A run of steps " + strconv.Itoa(index+1),
		}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("create an issue in %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
		keys[index] = decodeBody[struct {
			Key string `json:"key"`
		}](t, created).Key
	}
	return keys
}

// requireStepRefusal fails unless response is status with code, its message naming each of words.
func requireStepRefusal(t *testing.T, what string, response *httptest.ResponseRecorder, status int, code string, words ...string) {
	t.Helper()
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != status || refusal.Code != code {
		t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
	for _, word := range words {
		if !strings.Contains(refusal.Error, word) {
			t.Fatalf("%s: the refusal %q does not name %q", what, refusal.Error, word)
		}
	}
}

// stepJSON is a record as the wire carries it, so two records compare field by field and a
// difference prints whole.
func stepJSON(t *testing.T, step model.StepRecord) string {
	t.Helper()
	encoded, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("encode step %s: %v", step.ID, err)
	}
	return string(encoded)
}

// A start and then an end of one run, as the daemon writes them, read back by a person: one task,
// one record holding the start's fields and the end's, and its attempt. The end's answer is the
// record as the read lists it.
func TestTheDaemonWritesAStepAndAReaderReadsIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	id := uuid.NewString()

	started := putStepOK(t, handler, issue, id, implementerStart(daemonSession("LEG")))
	if started.StartedAt == nil || !started.StartedAt.Equal(stepStart) || started.Result != "" || started.EndedAt != nil {
		t.Fatalf("the start's answer is %s, want the run started at %s and not ended", stepJSON(t, started), stepStart)
	}
	ended := putStepOK(t, handler, issue, id, implementerEnd(daemonSession("LEG"), "passed", stepEnd))

	read := readSteps(t, handler, issue)
	if read.Issue != issue || len(read.Tasks) != 1 || read.Tasks[0].Task != "default" || len(read.Tasks[0].Steps) != 1 {
		t.Fatalf("the read of %s is %+v, want the one task default with one step", issue, read)
	}
	want := model.StepRecord{
		ID: id, Issue: issue, Task: "default", Step: 2, Kind: "step", Role: "implementer",
		Type: "implementation", Agent: "legion-leg-leg-1-implementer", Model: "anthropic/claude-opus-5",
		Generation: 1, Attempt: 1, StartedAt: &stepStart, EndedAt: &stepEnd, Result: "passed",
	}
	if got := stepJSON(t, read.Tasks[0].Steps[0]); got != stepJSON(t, want) {
		t.Fatalf("the read lists\n%s\nwant\n%s", got, stepJSON(t, want))
	}
	if got := stepJSON(t, ended); got != stepJSON(t, want) {
		t.Fatalf("the end's answer is\n%s\nwant the record as the read lists it\n%s", got, stepJSON(t, want))
	}
	// A record whose run never reported answers no report and no step it returned to, as null.
	raw := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice").Body.String()
	for _, absent := range []string{`"returned_to":null`, `"report":null`} {
		if !strings.Contains(raw, absent) {
			t.Fatalf("the read %s does not carry %s", raw, absent)
		}
	}
}

// The read answers each task once, in the order of its first step, with its steps in the order
// they ran.
func TestTheReadListsTasksInTheOrderOfTheirFirstStep(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
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
	issue := stepIssues(t, handler, "LEG", 1)[0]
	path := "/api/v1/issues/" + issue + "/steps/"

	person := dispatchRequest(t, handler, http.MethodPut, path+uuid.NewString(), implementerStart(nil), "alice")
	requireStepRefusal(t, "a person's write", person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	for _, session := range []string{legionDaemonSessionPrefix + "OTHER", "ses_agent"} {
		refused := putStep(t, handler, issue, uuid.NewString(), implementerStart(map[string]any{"kind": "session", "id": session}))
		requireStepRefusal(t, "a write as "+session, refused, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}

	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSession("LEG")))
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != id {
		t.Fatalf("the daemon's write left %+v, want the one record %s", steps, id)
	}
}

// A closed issue takes the record of the merge that closed it, where a handoff, which a running
// role writes, is refused there.
func TestAClosedIssueTakesItsMergeRecord(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	closeIssue(t, handler, issue)

	assertRefusedOnClosedIssue(t, "a handoff", postHandoff(t, handler, issue, "implement", `{}`))
	id := uuid.NewString()
	merged := putStepOK(t, handler, issue, id, map[string]any{
		"actor": daemonSession("LEG"), "task": "default", "step": 0, "kind": "merge", "role": "merger",
		"generation": 1, "result": "passed", "ended_at": stepEnd,
	})
	if merged.Kind != "merge" || merged.Step != 0 || merged.Agent != "" || merged.Result != "passed" || merged.Attempt != 1 {
		t.Fatalf("the merge record is %s, want a merge at step 0 with no agent, passed, attempt 1", stepJSON(t, merged))
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
	stepIssues(t, handler, "LEG", 1)

	missing := putStep(t, handler, "LEG-999", uuid.NewString(), implementerStart(daemonSession("LEG")))
	requireStepRefusal(t, "a write to LEG-999", missing, http.StatusNotFound, "ISSUE_NOT_FOUND", "LEG-999")
	unrouted := sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/LEG-999/no-such-records/"+uuid.NewString(),
		implementerStart(daemonSession("LEG")))
	if unrouted.Code != http.StatusNotFound || strings.Contains(unrouted.Body.String(), "ISSUE_NOT_FOUND") {
		t.Fatalf("a write to a route no row serves: status=%d body=%s, want a 404 without ISSUE_NOT_FOUND", unrouted.Code, unrouted.Body.String())
	}
}

// The read of an issue Dispatch does not hold answers as the read of its handoffs does: 200 and
// nothing in it.
func TestAnUnknownIssueReadsAsItsHandoffsRead(t *testing.T) {
	handler := newTestHandler(t)

	handoffs := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/LEG-999/handoffs", nil, "alice")
	if handoffs.Code != http.StatusOK || strings.TrimSpace(handoffs.Body.String()) != "[]" {
		t.Fatalf("the handoffs of LEG-999: status=%d body=%s, want 200 []", handoffs.Code, handoffs.Body.String())
	}
	steps := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/LEG-999/steps", nil, "alice")
	if want := `{"issue":"LEG-999","tasks":[]}`; steps.Code != http.StatusOK || strings.TrimSpace(steps.Body.String()) != want {
		t.Fatalf("the steps of LEG-999: status=%d body=%s, want 200 %s", steps.Code, steps.Body.String(), want)
	}
}

// A step write appends no event to its issue, whose activity a handoff moves.
func TestAStepWriteAppendsNoEvent(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	before := issueEventTypes(t, handler, issue)

	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSession("LEG")))
	putStepOK(t, handler, issue, id, implementerEnd(daemonSession("LEG"), "failed", stepEnd))
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
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
	start := implementerStart(daemon)
	noRole := maps.Clone(start)
	delete(noRole, "role")

	for _, row := range []struct {
		name, id string
		body     map[string]any
		field    string
	}{
		{"a kind that is no kind", "", with(start, map[string]any{"kind": "phase"}), "kind"},
		{"a result that is no result", "", with(start, map[string]any{"result": "done", "ended_at": stepEnd}), "result"},
		{"an end with no result", "", with(start, map[string]any{"ended_at": stepEnd}), "ended_at"},
		{"a step returned to by a run that passed", "", with(start, map[string]any{"result": "passed", "ended_at": stepEnd, "returned_to": 2}), "returned_to"},
		{"a first write with no role", "", noRole, "role"},
		{"a report that is no uuid", "", with(start, map[string]any{"result": "passed", "ended_at": stepEnd, "report": "plan-1"}), "report"},
		{"a task that is no task id", "", with(start, map[string]any{"task": "Default"}), "task"},
		{"an id that is no uuid", "not-a-uuid", start, "id"},
	} {
		t.Run(row.name, func(t *testing.T) {
			id := cmp.Or(row.id, uuid.NewString())
			requireStepRefusal(t, row.name, putStep(t, handler, issue, id, row.body), http.StatusBadRequest, codeStepInput, row.field)
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
			requireStepRefusal(t, name, response, http.StatusBadRequest, "INVALID_JSON")
		})
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 0 {
		t.Fatalf("the refused writes left %d records, want none", len(steps))
	}
}

// The start and the end of one new run, sent at once, both land, and the record holds the fields
// of each: the second write waits for the first and reads what it wrote.
func TestTwoPutsOfOneStepAtOneTimeKeepBoth(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	daemon := daemonSession("LEG")
	start := with(stepIdentity(daemon), map[string]any{"agent": "legion-leg-leg-1-implementer", "started_at": stepStart})
	end := with(stepIdentity(daemon), map[string]any{"result": "passed", "ended_at": stepEnd, "model": "anthropic/claude-opus-5"})

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
			t.Fatalf("round %d: the read lists %s, want the start's started_at and agent beside the end's result, ended_at and model", round, stepJSON(t, step))
		}
	}
	if len(steps) != rounds {
		t.Fatalf("the read lists %d records, want one for each of the %d rounds", len(steps), rounds)
	}
}

// A second end of one run lands and changes nothing: the run keeps its first end.
func TestAnEndPutTwiceKeepsItsFirstEnd(t *testing.T) {
	handler := newTestHandler(t)
	issue := stepIssues(t, handler, "LEG", 1)[0]
	id := uuid.NewString()
	putStepOK(t, handler, issue, id, implementerStart(daemonSession("LEG")))
	putStepOK(t, handler, issue, id, implementerEnd(daemonSession("LEG"), "stopped", stepEnd))

	again := putStepOK(t, handler, issue, id, implementerEnd(daemonSession("LEG"), "passed", stepEnd.Add(time.Hour)))
	if again.Result != "stopped" || again.EndedAt == nil || !again.EndedAt.Equal(stepEnd) {
		t.Fatalf("the second end answers %s, want the first end, stopped at %s", stepJSON(t, again), stepEnd)
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].Result != "stopped" || !steps[0].EndedAt.Equal(stepEnd) {
		t.Fatalf("the read lists %+v, want the one run ended stopped at %s", steps, stepEnd)
	}
}

// An id the record of one issue holds is refused to a write that names another issue.
func TestAStepOfAnotherIssueIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issues := stepIssues(t, handler, "LEG", 2)
	id := uuid.NewString()
	putStepOK(t, handler, issues[0], id, implementerStart(daemonSession("LEG")))

	other := putStep(t, handler, issues[1], id, implementerEnd(daemonSession("LEG"), "passed", stepEnd))
	requireStepRefusal(t, "the same id on "+issues[1], other, http.StatusConflict, codeStepOtherIssue, issues[0], issues[1])
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
		implementerStart(daemonSession("OTH"))))
	requireStepRefusal(t, "a write to "+other, refused, http.StatusForbidden, codeProjectScope, brokerTestProject, "OTH")
	answered := rig.serve(caller.presenting(t, http.MethodPut, "/api/v1/issues/"+own+"/steps/"+uuid.NewString(), token,
		implementerStart(daemonSession(brokerTestProject))))
	if answered.Code != http.StatusOK {
		t.Fatalf("the same write to %s: status=%d body=%s, want 200", own, answered.Code, answered.Body.String())
	}
}
