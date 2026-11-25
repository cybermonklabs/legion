package cireconcile

import (
	"strconv"
	"testing"
)

// verdictApp is the verdict App's id, as GitHub's own JSON carries it: the App that writes
// `legion-verdict` and that GitHub makes a check suite for on every push.
const verdictApp = 5341240

// A head settles beside the verdict App's queued suite, which the listener leaves out of its
// record. The App's run then completes that suite: the listener leaves the completed suite out
// too, and a reconcile pass that reads it from GitHub's listing leaves it out as well
// (DISPATCH_VERDICT_APP_ID), so the record does not move and the head is published once. A pass
// that recorded the suite would change the settled record, which re-arms it, and the head would
// be published a second time.
func TestAPassOverTheVerdictAppsCompletedSuitePublishesTheHeadOnce(t *testing.T) {
	h := newHarnessBeforeTheListener(t, reviewerAppID)
	h.verdict = strconv.Itoa(verdictApp)
	h.reconciler.VerdictAppID = h.verdict
	h.startListener()
	h.seedProjectDefault(rowRepository)
	h.listHead()
	run := checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")
	suite := checkSuite(900, "completed", "success", otherApp, "2026-11-05T03:01:05Z")
	verdictQueued := checkSuite(901, "queued", "", verdictApp, "2026-11-05T03:00:00Z")
	verdictCompleted := checkSuite(901, "completed", "success", verdictApp, "2026-11-05T03:05:00Z")
	settlements := h.subscribe()

	h.post("check_run", webhookBody("check_run", run), "d1")
	h.post("check_suite", webhookBody("check_suite", suite), "d2")
	h.post("check_suite", webhookBody("check_suite", verdictQueued), "d3")
	settled := h.settlement(settlements, "beside the verdict App's queued suite")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	h.post("check_suite", webhookBody("check_suite", verdictCompleted), "d4")
	_, before := h.settledRecord()

	h.github.setCheckRuns(gitHubRepository, headSHA, run)
	h.github.setCheckSuites(gitHubRepository, headSHA, suite, verdictCompleted)
	h.pass()

	if asked := h.queriesFor(gitHubRepository); asked != 1 {
		t.Fatalf("the repository was queried %d times, want once: the pass must read the verdict App's suite", asked)
	}
	h.noSettlement(settlements, "after a pass over the verdict App's completed suite")
	state, after, ok := h.record()
	if !ok {
		t.Fatal("the record is gone after the pass")
	}
	if after != before {
		t.Fatalf("the record's revision moved from %d to %d; the pass wrote the verdict App's suite: %+v", before, after, state.Suites)
	}
	if _, held := state.Suites["901"]; held {
		t.Fatalf("the record holds the verdict App's suite: %+v", state.Suites)
	}
}
