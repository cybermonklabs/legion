// check_states_lifecycle_test.go holds the settler to the lifecycle of a pull request's former
// heads: a close ends every association of the lifecycle it closes, so neither a healthy read of
// the pull request's commits nor a result recorded during that lifecycle brings one back after a
// reopen, while a result recorded once the close ended it, and a head the reopened pull request
// truly visits, count in the new lifecycle; and an association accepted in a lifecycle lasts to
// its close, whatever the pull request's commits, its base branch, its results or its checks' words
// do meanwhile. The rig is check_states_test.go's.
package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// eachHeadH5 is one more commit of #70: a commit pushed while the pull request was closed.
const eachHeadH5 = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"

// stateAt is the state the settler published for check at head of the rig's repository on issue,
// "" where it published none.
func stateAt(t *testing.T, rig *checkStatesRig, issue, head, check string) string {
	t.Helper()
	row, held := publishedRow(t, rig.database, issue, ciJobRepository, head, check)
	if !held {
		return ""
	}
	return row.state
}

// TestAReopenBringsBackNoHeadOfTheLifecycleItsCloseEnded: #70 visits H1, where the tester records
// k1 passing, and moves to H2: H1 is a former head and k1 is published yes there. #70 closes,
// which ends that lifecycle and deletes the rows of both heads, and reopens at H3. GitHub lists H1,
// H2 and H3 as #70's commits, a healthy read that names every head of the closed lifecycle, and
// the result at H1 is still recorded: neither makes H1 a former head of the reopened pull request,
// which never visited it, so nothing is published at H1 or H2 again. Once the reopened #70 does
// visit H1, moving to it and on to H4, H1 is a former head of the new lifecycle, and the result
// there publishes k1 yes again.
func TestAReopenBringsBackNoHeadOfTheLifecycleItsCloseEnded(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1, which #70 visited and left, is published as %q before the close, want yes: this test holds nothing", got)
	}
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2, eachHeadH3)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "closed", rig.issue)
	settleOwed(t, rig)
	wantNoRowAt(t, "#70 closed", rig, eachHeadH1, "", "k1")

	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != "" {
		t.Fatalf("after #70 closed and reopened at H3, k1 is published %q at H1, a head of the lifecycle the close ended, which GitHub still lists and a result names; want nothing there", got)
	}
	wantNoRowAt(t, "#70 reopened at H3", rig, eachHeadH2, "", "k1", "k2")

	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	upsertHead(t, rig, 70, eachHeadH4, rig.issue, "A change")
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("after the reopened #70 visited H1 and moved on to H4, k1 is published %q at H1, want yes: a head the new lifecycle visited is its former head", got)
	}
}

// TestAResultOfTheClosedPeriodCountsAfterAReopenAndAResultOfTheEndedLifecycleDoesNot: #70 visits
// H1, where the tester records k1 passing, moves to H2, and closes. While it is closed someone
// pushes H5 to its branch and the tester records k2 passing at H5. #70 reopens at H3, and GitHub
// lists H1, H2, H5 and H3 as its commits. H5's result was recorded after the close ended the
// lifecycle H1 belonged to, so H5 is a candidate of the new lifecycle, which the read confirms,
// and k2 is published yes there; H1's result was recorded in the lifecycle the close ended, so
// nothing is published at H1.
func TestAResultOfTheClosedPeriodCountsAfterAReopenAndAResultOfTheEndedLifecycleDoesNot(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	upsertHead(t, rig, 70, eachHeadH1, rig.issue, "A change")
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	settleOwed(t, rig)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH2, "closed", rig.issue)
	settleOwed(t, rig)

	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "pass", eachHeadH5)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2, eachHeadH5, eachHeadH3)
	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH5, "k2"); got != model.PassStandsYes {
		t.Fatalf("k2 at H5, recorded while #70 was closed and listed among its commits after it reopened, is published %q, want yes", got)
	}
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != "" {
		t.Fatalf("k1 at H1, recorded in the lifecycle #70's close ended, is published %q after the reopen, want nothing", got)
	}
}

// TestAnAcceptedFormerHeadLastsUntilTheClose: the install left H1 as #70's candidate, where the
// tester recorded k1 passing before it, and the read of #70's commits accepted it: k1 is published
// yes at H1. Then the branch is force-pushed, so GitHub lists H1 no more; the base branch takes
// H1; the tester's pass at H1 is retracted; and k1's words change. Through each, H1 stays a former
// head of #70 and the settler keeps publishing there: the retraction publishes k1 at H1 back to no,
// and a fail of k2 at H1 publishes no. Only #70's close ends it.
func TestAnAcceptedFormerHeadLastsUntilTheClose(t *testing.T) {
	rig := newCheckStatesRig(t, checkStatesTwoChecks)
	passed := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	upsertHead(t, rig, 70, eachHeadH2, rig.issue, "A change")
	forgetCandidates(t, rig)
	settleOwed(t, rig)
	rig.github.setPullCommits(70, eachHeadH1, eachHeadH2)
	seedInstallCandidates(t, rig, eachHeadH1)
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("k1 at H1, a candidate #70's commits list, is published %q, want yes: this test holds nothing", got)
	}
	reads := rig.github.pullCommitReadsOf(70)

	rig.github.setPullCommits(70, eachHeadH3)
	upsertHead(t, rig, 70, eachHeadH3, rig.issue, "A change")
	rig.github.setPull(70, eachHeadH3, "main", ciJobBaseSHA)
	rig.github.setCompare(ciJobBaseSHA, eachHeadH1, ciJobCompare{status: "behind"})
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsYes {
		t.Fatalf("after a force-push and a base branch that took H1, k1 at H1 is published %q, want yes still", got)
	}
	if response := retractAsPerson(t, rig.handler, passed, "Recorded against the wrong build."); response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("retract the pass at H1: status=%d body=%s", response.Code, response.Body.String())
	}
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k1"); got != model.PassStandsNo {
		t.Fatalf("after the pass at H1 was retracted, k1 at H1 is published %q, want no: H1 is still a former head", got)
	}
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md",
		checkStatesBlock("k1", ciCheckOrdinary, "The server records each result against a check.")+"\n"+
			checkStatesBlock("k2", ciCheckOrdinary, "The server answers a read of the results."))
	settleOwed(t, rig)
	eachHeadResult(t, rig.handler, rig.issue, "k2", "tester", "fail", eachHeadH1)
	settleOwed(t, rig)
	if got := stateAt(t, rig, rig.issue, eachHeadH1, "k2"); got != model.PassStandsNo {
		t.Fatalf("after k1's words changed, a fail of k2 at H1 is published %q, want no: H1 is still a former head", got)
	}
	if got := rig.github.pullCommitReadsOf(70); got != reads {
		t.Fatalf("#70's commits were read %d more times after H1 was accepted, want none: an accepted former head is not read again", got-reads)
	}
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "closed", rig.issue)
	settleOwed(t, rig)
	wantNoRowAt(t, "#70 closed", rig, eachHeadH1, "", "k1", "k2")
}
