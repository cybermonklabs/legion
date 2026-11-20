// ci_job_retract_test.go holds POST /api/v1/results/{id}/retract for a result a CI job recorded:
// it is retracted as any result is, by the session that recorded it, the session holding the
// Dispatch claim of the issue it was recorded on, or a person, with a reason, in a record of its
// own. A result is appended and never edited (R7), and a retraction is an appended record.
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheClaimHolderRetractsACIJobsFail: the job failed c1 and then passed it at one commit, the
// head of the job's branch, so c1 reads unknown (pass_and_fail). A session that is none of the
// three callers asks to retract the job's fail and is refused 403 RETRACTION_FORBIDDEN, naming the
// issue's claim: no retraction is stored, and c1 still reads unknown. The same session, holding
// the issue's claim now, retracts it: the retraction is stored against the job's fail, appended
// as result.retracted on the issue, and carried on the fail by the results read, and c1 reads as
// the remaining result gives it, yes from ci at the branch's head. A second retraction is 409
// RESULT_RETRACTED.
func TestTheClaimHolderRetractsACIJobsFail(t *testing.T) {
	rig := newCIJobRig(t)
	rig.github.setBranch(ciJobBranch, ciJobBranchHead{sha: ciJobCommit})
	failed := jobRecorded(t, "the first attempt's fail", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "fail")))
	jobRecorded(t, "the second attempt's pass", rig.post(t, rig.issue, jobTokenOn(rig, ciJobCommit, ciJobRef, 98), jobResultOf("c1", "pass")))
	unknown := func(when string) {
		t.Helper()
		if stands := passStandsOnWire(t, rig.handler, rig.issue, "c1"); stands.Answer != model.PassStandsUnknown || stands.Reason != model.PassStandsReasonPassAndFail {
			t.Fatalf("%s c1 reads %+v, want unknown for pass_and_fail", when, stands)
		}
	}
	unknown("before any retraction")
	retract := func() *httptest.ResponseRecorder {
		return rig.serve(rig.caller.presenting(t, http.MethodPost, "/api/v1/results/"+failed.ID+"/retract",
			rig.sessionToken(), map[string]any{"reason": "flaky"}))
	}

	refused := retract()
	if code, text := refusalOf(t, refused); refused.Code != http.StatusForbidden || code != "RETRACTION_FORBIDDEN" || !strings.Contains(text, rig.issue) {
		t.Fatalf("a session holding no claim retracts the job's fail: status=%d body=%s, want 403 RETRACTION_FORBIDDEN naming the claim of %s",
			refused.Code, refused.Body.String(), rig.issue)
	}
	if _, held := storedRetraction(t, rig.database, failed.ID); held {
		t.Fatal("a retraction of the job's fail is stored after the refusal")
	}
	unknown("after the refused retraction")

	claimIssueFor(t, rig.database, rig.issue, brokerTestSession)
	if taken := retract(); taken.Code != http.StatusCreated {
		t.Fatalf("the claim holder retracts the job's fail: status=%d body=%s, want 201", taken.Code, taken.Body.String())
	}
	if retraction, held := storedRetraction(t, rig.database, failed.ID); !held || retraction.Reason != "flaky" || retraction.Session != brokerTestSession {
		t.Fatalf("the stored retraction of the job's fail is %+v (held %v), want the reason flaky from %s", retraction, held, brokerTestSession)
	}
	if events := retractionEventsOn(t, rig.database, rig.issue); events != 1 {
		t.Fatalf("%s holds %d result.retracted events, want 1", rig.issue, events)
	}
	// readResults puts recorded_results back into results, where the job's own results are.
	check := checkOf(t, readResults(t, rig.handler, rig.issue), "c1")
	carried := []*model.CheckResultRetracted{}
	for _, result := range check.Results {
		if result.ID == failed.ID {
			carried = append(carried, result.Retracted)
		}
	}
	if len(carried) != 1 || carried[0] == nil || carried[0].Reason != "flaky" {
		t.Fatalf("the read carries the job's fail %d times with retracted %+v, want once with the reason flaky", len(carried), carried)
	}
	passStandsFrom(t, check, "after the claim holder's retraction of the job's fail", model.PassStandsYes, "ci")
	if stands := passStandsOnWire(t, rig.handler, rig.issue, "c1"); subjectAt(t, stands, ciJobCommit).Answer != model.PassStandsYes {
		t.Fatalf("after the retraction c1 reads %+v, want yes at the head of %s", stands, ciJobBranch)
	}

	again := retract()
	if code, _ := refusalOf(t, again); again.Code != http.StatusConflict || code != "RESULT_RETRACTED" {
		t.Fatalf("a second retraction of the job's fail: status=%d body=%s, want 409 RESULT_RETRACTED", again.Code, again.Body.String())
	}
}
