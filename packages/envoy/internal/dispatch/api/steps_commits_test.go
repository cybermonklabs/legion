package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The commits of a run, as the Legion daemon writes them onto the run's record through a route of
// its own (PUT /api/v1/issues/{key}/steps/{id}/commits): the commit the task's head was at when
// the run started and when it ended, and for a fix, the block id of the CI check it fixes. The
// read of the issue's steps serves them, and the read of a project counts its fixes.

// Three commits of one task's head, in the order the head moved.
const (
	headX = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	headY = "b2c3d4e5f60718293a4b5c6d7e8f9012345678a1"
	headZ = "c3d4e5f60718293a4b5c6d7e8f9012345678a1b2"
)

// putCommits writes body as the commits of the step record id of issue.
func putCommits(t *testing.T, handler http.Handler, issue, id string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+id+"/commits", body)
}

func putCommitsOK(t *testing.T, handler http.Handler, issue, id string, body any) model.StepRecord {
	t.Helper()
	response := putCommits(t, handler, issue, id, body)
	if response.Code != http.StatusOK {
		t.Fatalf("write the commits of step %s of %s: status=%d body=%s, want 200", id, issue, response.Code, response.Body.String())
	}
	return decodeBody[model.StepRecord](t, response)
}

// held is the text a nullable member of a record holds, "null" for none, so a check prints both.
func held(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// A run's record takes its commits through their route, each once: the start at X and the end at
// Y land beside the run's own start and end, a second end at Z changes nothing, and the read lists
// X and Y. The fix, the step's second run, takes the check it fixes, and the read lists it on that
// run alone. A record written with none of the three serves each as null. The step route takes no
// commit: a body that names one is written without it.
func TestTheCommitsRouteWritesEachFieldOnce(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	first, fix, tester := uuid.NewString(), uuid.NewString(), uuid.NewString()

	putStepOK(t, handler, issue, first, implementerStart(daemon))
	started := putCommitsOK(t, handler, issue, first, withFields(stepIdentity(daemon), map[string]any{"start_commit": headX}))
	if held(started.StartCommit) != headX || started.EndCommit != nil || started.StartedAt == nil {
		t.Fatalf("the start commit answers %s, want start_commit %s beside the run's start and no end_commit", asJSON(t, started), headX)
	}
	putStepOK(t, handler, issue, first, implementerEnd(daemon, "failed", stepEnd))
	ended := putCommitsOK(t, handler, issue, first, withFields(stepIdentity(daemon), map[string]any{"end_commit": headY}))
	if held(ended.StartCommit) != headX || held(ended.EndCommit) != headY || ended.Result != "failed" {
		t.Fatalf("the end commit answers %s, want %s to %s beside the run's result", asJSON(t, ended), headX, headY)
	}
	again := putCommitsOK(t, handler, issue, first, withFields(stepIdentity(daemon), map[string]any{"end_commit": headZ}))
	if held(again.EndCommit) != headY {
		t.Fatalf("a second end at %s answers end_commit %s, want the first, %s", headZ, held(again.EndCommit), headY)
	}

	putStepOK(t, handler, issue, fix, withFields(implementerStart(daemon), map[string]any{"started_at": stepEnd}))
	fixed := putCommitsOK(t, handler, issue, fix, withFields(stepIdentity(daemon), map[string]any{"start_commit": headY, "fix_check": "k-ci"}))
	if held(fixed.FixCheck) != "k-ci" || held(fixed.StartCommit) != headY || fixed.Attempt != 2 {
		t.Fatalf("the fix's commits answer %s, want the step's second run, a fix of k-ci started at %s", asJSON(t, fixed), headY)
	}
	unwritten := putStepOK(t, handler, issue, tester, withFields(implementerStart(daemon), map[string]any{
		"step": 3, "role": "tester", "type": "testing", "start_commit": headZ, "end_commit": headZ, "fix_check": "k-ci",
	}))
	if unwritten.StartCommit != nil || unwritten.EndCommit != nil || unwritten.FixCheck != nil {
		t.Fatalf("the step route answers %s, want no commit and no check: the commits have a route of their own", asJSON(t, unwritten))
	}

	for _, want := range []struct{ id, start, end, check string }{
		{first, headX, headY, "null"},
		{fix, headY, "null", "k-ci"},
		{tester, "null", "null", "null"},
	} {
		record := stepOf(t, handler, issue, want.id)
		if got := []string{held(record.StartCommit), held(record.EndCommit), held(record.FixCheck)}; strings.Join(got, " ") != strings.Join([]string{want.start, want.end, want.check}, " ") {
			t.Errorf("the read lists run %s with start_commit, end_commit, fix_check %v, want %s %s %s", want.id, got, want.start, want.end, want.check)
		}
	}
	raw := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/steps", nil, "alice").Body.String()
	for _, member := range []string{`"start_commit":null`, `"end_commit":null`, `"fix_check":null`} {
		if !strings.Contains(raw, member) {
			t.Fatalf("the read %s does not carry %s for a record that holds none", raw, member)
		}
	}
}

// A commit is 40 lowercase hexadecimal digits, and only a run of kind step names a fix_check, by
// the kind the body names and by the kind the record holds: each other value is 400 STEP_INPUT
// naming the field, with nothing written. A fix_check holding U+0000 is refused by the body's
// decoder, 400 NUL_CHARACTER. The same writes with each value in its form land, which is the
// control: a block id holding a space and a colon is a fix_check.
func TestACommitsWriteRefusesItsBadValues(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	daemon := daemonSessionOf("LEG")
	retro := uuid.NewString()
	putStepOK(t, handler, issue, retro, withFields(retroIdentity(daemon, 1), map[string]any{"started_at": stepStart}))

	for _, row := range []struct {
		name string
		id   string
		body map[string]any
		code string
		says string
	}{
		{"a start_commit of 39 digits", uuid.NewString(), withFields(stepIdentity(daemon), map[string]any{"start_commit": headX[:39]}), codeStepInput, "start_commit"},
		{"an end_commit in upper case", uuid.NewString(), withFields(stepIdentity(daemon), map[string]any{"end_commit": strings.ToUpper(headY)}), codeStepInput, "end_commit"},
		{"a fix_check of a retro", uuid.NewString(), withFields(retroIdentity(daemon, 1), map[string]any{"fix_check": "k-ci"}), codeStepInput, "fix_check"},
		{"a fix_check of a merge", uuid.NewString(), withFields(retroIdentity(daemon, 1), map[string]any{"kind": "merge", "fix_check": "k-ci"}), codeStepInput, "fix_check"},
		{"a fix_check of a record held as a retro", retro, map[string]any{"actor": daemon, "fix_check": "k-ci"}, codeStepInput, "fix_check"},
		{"a fix_check holding U+0000", uuid.NewString(), withFields(stepIdentity(daemon), map[string]any{"fix_check": "k\x00ci"}), "NUL_CHARACTER", "fix_check"},
	} {
		t.Run(row.name, func(t *testing.T) {
			expectRefusal(t, row.name, putCommits(t, handler, issue, row.id, row.body), http.StatusBadRequest, row.code, row.says)
		})
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != retro || steps[0].FixCheck != nil {
		t.Fatalf("the refused writes left %s, want the one retro and no check", asJSON(t, steps))
	}

	landed := putCommitsOK(t, handler, issue, uuid.NewString(), withFields(stepIdentity(daemon), map[string]any{
		"start_commit": headX, "end_commit": headY, "fix_check": "check 1: the unit job",
	}))
	if held(landed.StartCommit) != headX || held(landed.EndCommit) != headY || held(landed.FixCheck) != "check 1: the unit job" {
		t.Fatalf("the write in its forms answers %s, want each value as written", asJSON(t, landed))
	}
	retroStarted := putCommitsOK(t, handler, issue, retro, map[string]any{"actor": daemon, "start_commit": headX})
	if held(retroStarted.StartCommit) != headX || retroStarted.Kind != "retro" {
		t.Fatalf("a retro's commit answers %s, want the retro started at %s: any run takes its commits", asJSON(t, retroStarted), headX)
	}
}

// Only the daemon of the issue's project writes a run's commits, as only it writes the run's
// record: a person, an agent's session and the daemon of another project, each sending the body the
// daemon's own write lands with, are refused 403 STEP_WRITER, whether the record exists or not,
// and the record stays without a commit; the daemon's write of the same body lands, which is the
// control.
func TestTheCommitsRouteRefusesAnotherWriter(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createTestIssue(t, handler, "LEG", "A run of steps")
	recorded := uuid.NewString()
	putStepOK(t, handler, issue, recorded, implementerStart(daemonSessionOf("LEG")))
	commits := map[string]any{"start_commit": headX, "fix_check": "k-ci"}

	for _, id := range []string{recorded, uuid.NewString()} {
		path := "/api/v1/issues/" + issue + "/steps/" + id + "/commits"
		person := dispatchRequest(t, handler, http.MethodPut, path, withFields(stepIdentity(nil), commits), "alice")
		expectRefusal(t, "a person's commits of "+id, person, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
		for _, session := range []string{legionDaemonSessionPrefix + "OTHER", "ses_agent"} {
			refused := putCommits(t, handler, issue, id, withFields(stepIdentity(map[string]any{"kind": "session", "id": session}), commits))
			expectRefusal(t, "the commits of "+id+" as "+session, refused, http.StatusForbidden, codeStepWriter, "legion-daemon:LEG", issue)
		}
	}
	if steps := stepsOf(t, handler, issue); len(steps) != 1 || steps[0].ID != recorded || steps[0].StartCommit != nil || steps[0].FixCheck != nil {
		t.Fatalf("the refused writes left %s, want the one run %s, with no commit and no check", asJSON(t, steps), recorded)
	}

	written := putCommitsOK(t, handler, issue, recorded, withFields(stepIdentity(daemonSessionOf("LEG")), commits))
	if held(written.StartCommit) != headX || held(written.FixCheck) != "k-ci" {
		t.Fatalf("the daemon's write answers %s, want start_commit %s and fix_check k-ci", asJSON(t, written), headX)
	}
}

// The read of a project counts each finished issue's fixes, its step records that name a
// fix_check, beside its failed steps: an issue whose implementer's step ran twice more as a fix of
// k-ci, after its tester failed once, reads fixes 2 and failed_steps 1. The control is the same
// issue with the second fix written without its check, which reads fixes 1: a count of the step's
// runs, of its later attempts or of the commits alone would read 2 there too. An issue with no fix
// reads fixes 0.
func TestTheProjectReadCountsFixes(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name          string
		secondChecked bool
		fixes         int
	}{
		{"two runs naming a check", true, 2},
		{"the control: one of them names none", false, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			issues := createListedIssues(t, handler, "LEG", 2)
			fixed, plain := issues[0], issues[1]
			daemon := daemonSessionOf("LEG")
			recordRuns(t, handler, "LEG", fixed,
				run{"step", "implementer", 2, new(at(0)), new(at(10)), "passed"},
				run{"step", "tester", 3, new(at(10)), new(at(20)), "failed"},
			)
			for index, checked := range []bool{true, row.secondChecked} {
				id := recordRuns(t, handler, "LEG", fixed, run{"step", "implementer", 2, new(at(30 + 10*index)), new(at(35 + 10*index)), "passed"})[0]
				commits := map[string]any{"start_commit": headX, "end_commit": headY}
				if checked {
					commits["fix_check"] = "k-ci"
				}
				putCommitsOK(t, handler, fixed, id, withFields(stepIdentity(daemon), commits))
			}
			recordRuns(t, handler, "LEG", plain, run{"step", "implementer", 2, new(at(0)), new(at(10)), "passed"})
			for index, key := range []string{fixed, plain} {
				closeIssue(t, handler, key)
				if _, err := database.Pool.Exec(context.Background(), "update issues set closed_at = $2 where key = $1", key, at(1000+index)); err != nil {
					t.Fatalf("close %s: %v", key, err)
				}
			}

			read := readPlans(t, handler, "LEG", "")
			counts := map[string][2]int{}
			for _, issue := range read.Issues {
				counts[issue.Issue] = [2]int{issue.Fixes, issue.FailedSteps}
			}
			if counts[fixed] != [2]int{row.fixes, 1} || counts[plain] != [2]int{0, 0} {
				t.Fatalf("the read counts fixes and failed_steps %v, want %s %v and %s [0 0]", counts, fixed, [2]int{row.fixes, 1}, plain)
			}
			raw := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/LEG/plans", nil, "alice").Body.String()
			if !strings.Contains(raw, `"fixes":0`) {
				t.Fatalf("the read %s does not carry \"fixes\":0 for the issue with no fix", raw)
			}
		})
	}
}
