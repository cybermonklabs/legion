// The whole-daemon tests of an issue that closes when the checks of its goal pass, on the rig of
// done_by_checks_rig_test.go: a real Dispatch of this checkout that reads the rig's GitHub
// stand-in, the fake runtime, and the daemon's own store. TestAnIssueClosesWhenTheChecksOfItsGoalPass
// drives the root's check v-done, a merge that closes nothing, a fail that starts the next change,
// and the close once each check passes at the newest merge. TestANextChangeNeedsAPassAtItsOwnHead
// reads a before_merge check at the next change's own head. TestAWriteToItsOwnIssueIsTakenWhileTheIssueWaits
// and TestAChildFiledAfterTheBootHoldsItsRootsClose are an issue's own write while it waits and a
// child filed after the boot. Every wait is on a row the daemon or Dispatch wrote, which the log
// names.
package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// doneByChecksBeforeCheck is a check that runs before the merge, at each open pull request's
	// head, or at the newest merged one's final head where none is open.
	doneByChecksBeforeCheck = "c-before-merge"
	// doneByChecksSecondPull is the pull request of the first issue's next change, open on the
	// same branch at doneByChecksSecondHead, which GitHub merges as doneByChecksSecondMerge.
	doneByChecksSecondPull  = 42
	doneByChecksSecondHead  = "ae7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f42"
	doneByChecksSecondMerge = "3e46e00000000000000000000000000000000042"
	// doneByChecksPassCommit is a commit of main after the fail's (doneByChecksLaterCommit) that
	// holds the first merge and not the second; doneByChecksSecondPassCommit is one after the second
	// merge; doneByChecksLateFailCommit is the commit of a fail recorded after the close.
	doneByChecksPassCommit       = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	doneByChecksSecondPassCommit = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	doneByChecksLateFailCommit   = "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4"
	// The pull request of the second issue, of the third, and of the fifth, the third's child: each
	// its number, its head and its merge.
	doneByChecksGadgetsPull   = 51
	doneByChecksGadgetsHead   = "be7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f51"
	doneByChecksGadgetsMerge  = "3e46e00000000000000000000000000000000051"
	doneByChecksSprocketPull  = 61
	doneByChecksSprocketHead  = "ce7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f61"
	doneByChecksSprocketMerge = "3e46e00000000000000000000000000000000061"
	doneByChecksGearsPull     = 62
	doneByChecksGearsHead     = "de7f00d5a1b2c3d4e5f60718293a4b5c6d7e8f62"
	doneByChecksGearsMerge    = "3e46e00000000000000000000000000000000062"
	// doneByChecksOpenItems is the issue's own document that its check after the merge reads, and
	// doneByChecksOpenItemsCheck that check.
	doneByChecksOpenItems      = "open-items.md"
	doneByChecksOpenItemsCheck = "c-open-items"
)

// doneByChecksBothChecks is a Verification document of two checks: one that runs before the merge,
// and one after it of code (a block that names no subject reads as code).
const doneByChecksBothChecks = "# Verification\n\n:::check{#" + doneByChecksBeforeCheck +
	` surface="cli" when="before_merge" run="go test ./internal/widgets/"}` + "\nThe widgets pass their tests at the pull request's head.\n:::\n\n" +
	":::check{#" + doneByChecksCheck + ` surface="cli" when="after_merge" run="go test ./internal/widgets/"}` +
	"\nThe widgets report their merge once it reaches main.\n:::\n"

// doneByChecksBeforeOnly is a Verification document with no check after the merge.
const doneByChecksBeforeOnly = "# Verification\n\n:::check{#" + doneByChecksBeforeCheck +
	` surface="cli" when="before_merge" run="go test ./internal/widgets/"}` + "\nThe gadgets pass their tests at the pull request's head.\n:::\n"

// doneByChecksOpenItemsVerification is a Verification document whose one check runs after the
// merge and reads the issue's own document, doneByChecksOpenItems.
const doneByChecksOpenItemsVerification = "# Verification\n\n:::check{#" + doneByChecksOpenItemsCheck +
	` surface="cli" when="after_merge" run="go test ./internal/widgets/"}` +
	"\nThe open items of this issue name each issue with its merged pull request.\n:::\n"

// The issue's own document before the role's edit, and after it.
const (
	doneByChecksOpenItemsBefore = "# Open items\n\n- ACME-7: no merged pull request named\n"
	doneByChecksOpenItemsAfter  = "# Open items\n\n- ACME-7: merged as acme/widgets#7\n"
)

// v-done (dispatch://INFRA-3570/artifact/verification-md), and of this issue's checks the whole-daemon
// cases of c-merge-no-close, c-close-on-checks, c-fail-opens-change, c-wait-visible,
// c-running-issues and c-no-checks (plan Task 11b, Step 1).
//
// The first run's project runs a production check. The first issue's Verification document holds
// one check before the merge and one of code after it. Its merge closes nothing: it stands in
// verifying, with no slot and the status testing, and its production check's first look stays
// open with no report. A fail of the check after the merge, at a commit of main that holds the
// merge, starts its next change: generation 2 on the same branch, moved to main's head, with the
// working copy started at the branch before its first task, which names the check. With the next
// change's pull request open, the production check of the first merge reports pass on that look,
// and both checks pass, the one before the merge at the change's own head: the open change alone
// holds the close. The change merges. Its own production check opens, a pass of the check after
// the merge at a commit that holds the second merge is recorded, the check reports pass, and the
// issue closes, marked closed on its checks. A fail after the close tells the controller once and
// reopens nothing. The daemon posts no message on the issue from its entry into verifying to its
// close, and close-wait.md has a version for each change of the wait, the last naming the close.
//
// The second issue has no check after the merge: its production check reports pending, then pass,
// and it closes at that pass. The third issue's record is written before the boot at its review,
// each column of this release at its default: after its merge it stays open, as the first did. The
// fifth issue is the third's child: the architect that owns it sets it done after its merge, while
// its check after the merge has no pass, and the daemon sets that back and tells the architect.
//
// The second run's project runs no production check, and its one issue, the fourth, has no
// Verification document: it closes at the first read after its merge.
func TestAnIssueClosesWhenTheChecksOfItsGoalPass(t *testing.T) {
	t.Parallel()
	t.Run("a project that runs a production check", func(t *testing.T) {
		t.Parallel()
		r := newDoneByChecksRigWith(t, doneByChecksOptions{production: claim.RoleTester, issues: []doneByChecksIssue{
			{title: doneByChecksTitle, verification: doneByChecksBothChecks, number: doneByChecksPull, head: stepRecordsHead},
			{title: "Gadgets publish a changelog", verification: doneByChecksBeforeOnly, number: doneByChecksGadgetsPull, head: doneByChecksGadgetsHead},
			{title: "Sprockets rotate under load", verification: doneByChecksVerification, number: doneByChecksSprocketPull, head: doneByChecksSprocketHead, reviewing: true},
			{title: "Gears mesh with the new axle", verification: doneByChecksVerification, number: doneByChecksGearsPull, head: doneByChecksGearsHead, under: 3, reviewing: true},
		}})
		first, second, third, fifth := r.issues[0].key, r.issues[1].key, r.issues[2].key, r.issues[3].key
		thirdSeeded(r, third)
		look := firstToItsMerge(r, first)
		secondClosesAtItsProductionPass(r, second)
		r.on(third)
		r.walkReview(doneByChecksSprocketPull, doneByChecksSprocketHead)
		fifthSetBack(r, third, fifth)
		firstThroughItsNextChange(r, first, look)
		thirdStaysOpenAfterItsMerge(r, third, fifth)
		if got := r.d.state().Issues[second]; got.Phase != phase.Done {
			t.Errorf("%s stands in %s at the run's end, want done since its production check's pass", second, got.Phase)
		}
		r.standIn.logRequests()
	})
	t.Run("a project that runs no production check", func(t *testing.T) {
		t.Parallel()
		r := newDoneByChecksRigWith(t, doneByChecksOptions{issues: []doneByChecksIssue{
			{title: "Levers report no checks", number: doneByChecksPull, head: stepRecordsHead},
		}})
		fourth := r.key
		r.walkToMerging(doneByChecksPull, stepRecordsHead)
		merged := r.mergeOf(fourth, doneByChecksPull, doneByChecksMergeCommit)
		read := r.awaitCloseRead(fourth, "the merge of the fourth issue", merged, func(read doneByChecksRead) error {
			if len(read.answer.Results.Checks) != 0 {
				return fmt.Errorf("the read holds %d checks, want none: the issue has no Verification document", len(read.answer.Results.Checks))
			}
			return nil
		})
		r.awaitIssue(fourth, "the first read after its merge", inPhase(phase.Done))
		row := r.issueRow(fourth)
		if row.ClosedOnChecksAt == nil || row.ClosedOnChecksAt.Before(read.recordedAt) {
			t.Fatalf("%s closed on its checks at %v, want a close at the read stored at %s", fourth, row.ClosedOnChecksAt, read.recordedAt)
		}
		t.Logf("%s closed on its checks at %s, at the first read after its merge", fourth, row.ClosedOnChecksAt.UTC().Format(time.RFC3339Nano))
	})
}

// thirdSeeded holds the third issue's record as the boot found it: in reviewing at generation 1,
// with every column of this release at its default, as the migration leaves a row of the release
// before.
func thirdSeeded(r *doneByChecksRig, third string) {
	t := r.t
	t.Helper()
	row := r.issueRow(third)
	switch {
	case row.Phase != phase.Reviewing || row.Generation != 1:
		t.Fatalf("%s stands in %s at generation %d after the boot, want reviewing at 1", third, row.Phase, row.Generation)
	case len(row.CloseWait) != 0 || row.CloseWaitSince != nil || row.ClosedOnChecksAt != nil || row.Merge.Merged() ||
		row.NextChange != 0 || row.NextChanges != 0 || row.MergerCompletionOwed || row.StandingToldAt != nil || row.TreeGeneration != row.Generation:
		t.Fatalf("%s's columns of this release after the boot are %+v, want each at its default", third, row)
	}
	t.Logf("%s holds the record of the release before: reviewing at generation 1, each column of this release at its default", third)
}

// firstToItsMerge is items 1 and 2 of Task 11b's Step 1. The first issue walks to merging, with a
// pass of its check before the merge at its head, and its pull request merges while the merger
// works. The first read after the merge names the merge on main: the issue stands in verifying,
// status testing, with no slot, close-wait.md names what it waits for and since when, and the
// production check opens at that read. Its look 1 is told to the tester, whose turn ends with no
// report, so the production term reads open. It answers the check's opening and the count of the
// daemon's messages on the issue at its entry.
func firstToItsMerge(r *doneByChecksRig, first string) firstLook {
	t := r.t
	t.Helper()
	r.on(first)
	r.walkPlanning()
	r.walkImplementing(stepRecordsHead)
	r.walkTesting()
	r.recordCheck(doneByChecksBeforeCheck, "pass", stepRecordsHead)
	r.walkReview(doneByChecksPull, stepRecordsHead)
	r.walkSignOff()
	r.walkRetro(stepRecordsHead)
	r.awaitIssue(first, "its retro's end", func(issue api.Issue) error {
		if issue.Phase != phase.Merging || issue.Status != "retro" {
			return fmt.Errorf("it stands in %s with status %s, want merging with status retro", issue.Phase, issue.Status)
		}
		return nil
	})
	merged, status, body := r.mergeAndComplete(doneByChecksPull, doneByChecksMergeCommit)
	r.awaitCloseRead(first, "the first merge", merged, namesMerge(doneByChecksCheck, doneByChecksMergeCommit))
	r.awaitIssue(first, "the entry into verifying", func(issue api.Issue) error {
		switch {
		case issue.Phase != phase.Verifying || issue.Status != "testing":
			return fmt.Errorf("it stands in %s with status %s, want verifying with status testing", issue.Phase, issue.Status)
		case issue.Slot != nil:
			return fmt.Errorf("it holds a slot %+v, want none in verifying", *issue.Slot)
		}
		return nil
	})
	row := r.issueRow(first)
	if row.CloseWaitSince == nil || row.Merge.SHA != doneByChecksMergeCommit {
		t.Fatalf("%s's row after its merge waits since %v at merge %q, want the merge's time at %s", first, row.CloseWaitSince, row.Merge.SHA, doneByChecksMergeCommit)
	}
	wait := r.storedWait(first)
	if !slices.ContainsFunc(wait.Checks, func(check record.WaitCheck) bool { return check.ID == doneByChecksCheck }) || wait.Production == nil {
		t.Fatalf("%s's stored wait at its entry is %+v, want check %s and the production check", first, wait, doneByChecksCheck)
	}
	versions := r.awaitCloseWaitDocument(first, "the entry into verifying")
	newest := versions[len(versions)-1]
	for _, named := range []string{"`" + doneByChecksCheck + "`", closeWaitTime(*row.CloseWaitSince), doneByChecksMergeCommit} {
		if !strings.Contains(newest, named) {
			t.Fatalf("close-wait.md of %s at its entry names no %q", first, named)
		}
	}
	messages := r.daemonMessages(first)
	t.Logf("%s holds %d messages in Dispatch at its entry into verifying", first, messages)

	tester := r.worker(claim.RoleTester)
	opened := r.lookOf(tester, 1, stepRecordsWait)
	tester.endTurn()
	r.awaitIssue(first, "look 1 open with no report", func(issue api.Issue) error {
		production := productionOf(issue)
		if production == nil || production.State != record.ProductionOpen || production.Generation != 1 || production.Merge != doneByChecksMergeCommit {
			return fmt.Errorf("its production term is %+v, want open at generation 1 and merge %s", production, doneByChecksMergeCommit)
		}
		return nil
	})
	if check := readProductionCheck(t, r.st.Pool(), r.records, first); check.Looks != 1 || !check.LookOpen {
		t.Fatalf("%s's production check after its look's turn is %+v, want look 1 open", first, check)
	}
	return firstLook{opened: opened, messages: messages, completion: status, completionBody: body}
}

// firstLook is what firstToItsMerge leaves for the rest of the first issue's run: the opening of
// its production check, the count of the daemon's messages on it at its entry into verifying, and
// the daemon's answer to its merger's completion after the merge.
type firstLook struct {
	opened         time.Time
	messages       int
	completion     int
	completionBody string
}

// secondClosesAtItsProductionPass is the second issue, whose one check runs before the merge: it
// passes at the pull request's head, the issue merges and waits in verifying, its production
// check's look 1 reports pending with a wake, look 2 starts at that wake and reports pass, and the
// read after it closes the issue.
func secondClosesAtItsProductionPass(r *doneByChecksRig, second string) {
	t := r.t
	t.Helper()
	r.on(second)
	r.walkPlanning()
	r.walkImplementing(doneByChecksGadgetsHead)
	r.walkTesting()
	r.recordCheck(doneByChecksBeforeCheck, "pass", doneByChecksGadgetsHead)
	r.walkReview(doneByChecksGadgetsPull, doneByChecksGadgetsHead)
	r.walkSignOff()
	r.walkRetro(doneByChecksGadgetsHead)
	merged := r.mergeOf(second, doneByChecksGadgetsPull, doneByChecksGadgetsMerge)
	r.awaitCloseRead(second, "the second issue's merge", merged, passesAt(doneByChecksBeforeCheck, doneByChecksGadgetsHead))
	r.awaitIssue(second, "its merge, its production check open", func(issue api.Issue) error {
		if production := productionOf(issue); issue.Phase != phase.Verifying || production == nil || production.State != record.ProductionOpen {
			return fmt.Errorf("it stands in %s with its production term %+v, want verifying with the term open", issue.Phase, production)
		}
		return nil
	})
	tester := r.worker(claim.RoleTester)
	opened := r.lookOf(tester, 1, stepRecordsWait)
	wake := time.Now().Add(time.Second)
	r.reportLook(tester, opened, 1, "pending", &wake)
	tester.endTurn()
	r.awaitIssue(second, "the pending of look 1", func(issue api.Issue) error {
		if production := productionOf(issue); issue.Phase != phase.Verifying || production == nil || production.NextLookAt == nil {
			return fmt.Errorf("it stands in %s with its production term %+v, want verifying with its next look named", issue.Phase, production)
		}
		return nil
	})
	if again := r.lookOf(tester, 2, stepRecordsWait+workflow.CheckWakeFloor); !again.Equal(opened) {
		t.Fatalf("look 2 of %s names the check opened at %s, want %s, the check of look 1", second, again, opened)
	}
	passed := r.reportLook(tester, opened, 2, "pass", nil)
	tester.endTurn()
	r.awaitCloseRead(second, "the production check's pass", passed, passesAt(doneByChecksBeforeCheck, doneByChecksGadgetsHead))
	r.awaitIssue(second, "the production check's pass", inPhase(phase.Done))
}

// fifthSetBack is the fifth issue, the third's child, from its review to its merge, its pull
// request approved and signed off by the tree's architect, and then that architect's done on it
// while its check after the merge has no pass: the daemon sets the done back, the child stays in
// verifying with the status testing, and the architect is told why.
func fifthSetBack(r *doneByChecksRig, third, fifth string) {
	t := r.t
	t.Helper()
	r.on(third)
	r.reviewOf(fifth, doneByChecksGearsPull, doneByChecksGearsHead, "APPROVED", phase.Accepting)
	r.signOff(fifth)
	r.retroOf(fifth, doneByChecksGearsHead)
	merged := r.mergeOf(fifth, doneByChecksGearsPull, doneByChecksGearsMerge)
	r.awaitCloseRead(fifth, "the fifth issue's merge", merged, namesMerge(doneByChecksCheck, doneByChecksGearsMerge))
	r.awaitIssue(fifth, "its merge", inVerifying)
	told := func() int {
		count := 0
		for _, notice := range r.architectNotices(third, "close-waits") {
			if strings.Contains(notice.Payload, fifth) {
				count++
			}
		}
		return count
	}
	before := told()
	architect := r.worker(claim.RoleArchitect)
	seq := r.sessionStatus(fifth, architect, record.DoneStatus)
	r.awaitApplied("the owning architect's done on "+fifth, fifth, func(event doneByChecksEvent) bool { return event.Seq == seq })
	r.awaitIssue(fifth, "the architect's done", inVerifying)
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the architect of "+third+" told why "+fifth+" stays open", func() bool { return told() > before })
	var status struct {
		Status string `json:"status"`
	}
	testwait.WithinPolling(t, stepRecordsWait, 200*time.Millisecond, "Dispatch's status of "+fifth+" set back to testing", func() bool {
		return r.personRead("/api/v1/issues/"+fifth, &status) == nil && status.Status == "testing"
	})
	t.Logf("the architect's done on %s set back: Dispatch shows it %s, and %s's architect holds %d close-waits notices naming it", fifth, status.Status, third, told())
}

// firstThroughItsNextChange is items 3 to 8 of Task 11b's Step 1, on the first issue, whose look 1
// stands open with no report.
func firstThroughItsNextChange(r *doneByChecksRig, first string, look firstLook) {
	t := r.t
	t.Helper()
	r.on(first)
	branch := claim.IssueBranch(first)

	// Item 3: the fail at a commit of main that holds the merge starts the next change.
	later := r.standIn.commitOnMain(doneByChecksLaterCommit)
	fail := r.record("fail", later)
	failed := r.awaitResultFact(fail)
	r.awaitCloseRead(first, "the fail at "+later, failed, func(read doneByChecksRead) error {
		block, held := read.check(doneByChecksCheck)
		if !held || block.PassStands.Answer != dispatch.PassStandsNo || block.PassStands.Reason != dispatch.PassStandsReasonFail {
			return fmt.Errorf("%s answers %+v, want no for reason fail", doneByChecksCheck, block.PassStands)
		}
		return nil
	})
	r.awaitIssue(first, "the fail: the next change's start", func(issue api.Issue) error {
		if issue.Generation != 2 {
			return fmt.Errorf("it stands at generation %d in %s, want generation 2; the merger's completion after the merge answered %d %s",
				issue.Generation, issue.Phase, look.completion, look.completionBody)
		}
		return nil
	})
	if look.completion != http.StatusOK {
		t.Fatalf("the merger's completion of %s in verifying = %d %s, want 200", first, look.completion, look.completionBody)
	}
	planner := r.worker(claim.RolePlanner)
	prompt := r.taskOf(planner)
	moves := r.standIn.refWritesOf(branch)
	if len(moves) == 0 || moves[len(moves)-1].method != http.MethodPatch || moves[len(moves)-1].sha != later || moves[len(moves)-1].caller != "daemon" {
		t.Fatalf("the writes of %s on GitHub at the change's first task are %+v, want the daemon's PATCH to main's head %s", branch, moves, later)
	}
	if !slices.ContainsFunc(r.rt.Calls(), func(call fake.Call) bool {
		return call.Method == "StartAtBranch" && call.Locator.Claim == planner.token && call.StartAt == runtime.StartAt{Bookmark: branch, Base: "main"}
	}) {
		t.Fatalf("the fake runtime holds no start of %s's working copy at %s before its first task", planner.token, branch)
	}
	if !strings.Contains(prompt.Message, doneByChecksCheck) {
		t.Fatalf("the next change's first task of %s names no check %s", planner.token, doneByChecksCheck)
	}
	t.Logf("generation 2 of %s started: the daemon's PATCH of %s to %s, the fake runtime's StartAtBranch(%s, main) of %s, then its first task, which names %s",
		first, branch, later, branch, planner.token, doneByChecksCheck)
	r.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned the change"}, phase.Implementing)
	r.standIn.open(doneByChecksSecondPull, branch, doneByChecksSecondHead)
	opened := r.publishPull(doneByChecksSecondPull, "opened")
	t.Logf("the pull request's opened fact applied: processed_events row (source github, event_id %s, processed_at %s) of #%d at %s",
		opened, r.awaitProcessed("github", opened).UTC().Format(time.RFC3339Nano), doneByChecksSecondPull, doneByChecksSecondHead)
	r.awaitAnswerOf(doneByChecksBeforeCheck, fmt.Sprintf("#%d opened", doneByChecksSecondPull), subjectNamed(doneByChecksSecondHead))

	// Item 4: with the change's pull request open, each other input of the close passes.
	tester := r.worker(claim.RoleTester)
	tester.send(shimwire.AgentStart{})
	tester.waitState(supervise.StateWorking)
	r.reportLook(tester, look.opened, 1, "pass", nil)
	tester.send(shimwire.AgentEnd{})
	tester.waitState(supervise.StateIdle)
	if check := readProductionCheck(t, r.st.Pool(), r.records, first); check.Open() || check.Outcome != record.CheckRecorded || check.MergeSHA != doneByChecksMergeCommit {
		t.Fatalf("%s's production check after the pass of look 1 is %+v, want it closed recorded for %s", first, check, doneByChecksMergeCommit)
	}
	passBefore := r.recordCheck(doneByChecksBeforeCheck, "pass", doneByChecksSecondHead)
	passAfter := r.record("pass", r.standIn.commitOnMain(doneByChecksPassCommit))
	both := r.awaitResultFact(passBefore)
	if at := r.awaitResultFact(passAfter); at.After(both) {
		both = at
	}
	r.awaitCloseRead(first, "both passes", both, func(read doneByChecksRead) error {
		for _, check := range []string{doneByChecksBeforeCheck, doneByChecksCheck} {
			if block, held := read.check(check); !held || block.PassStands.Answer != dispatch.PassStandsYes {
				return fmt.Errorf("%s answers %+v, want yes", check, block.PassStands)
			}
		}
		return nil
	})
	r.awaitIssue(first, "both passes while its change is open", func(issue api.Issue) error {
		if issue.Generation != 2 || issue.Phase == phase.Done || issue.Phase == phase.Verifying {
			return fmt.Errorf("it stands in %s at generation %d, want generation 2 in a phase of its change", issue.Phase, issue.Generation)
		}
		return nil
	})

	// Item 5: the change runs to its merge, and the production check of that merge opens.
	r.walkImplementing(doneByChecksSecondHead)
	r.walkTesting()
	r.walkReview(doneByChecksSecondPull, doneByChecksSecondHead)
	r.walkSignOff()
	r.walkRetro(doneByChecksSecondHead)
	merged, status, body := r.mergeAndComplete(doneByChecksSecondPull, doneByChecksSecondMerge)
	if status != http.StatusOK {
		t.Fatalf("the merger's completion of the change of %s in verifying = %d %s, want 200", first, status, body)
	}
	r.awaitCloseRead(first, "the second merge", merged, func(read doneByChecksRead) error {
		block, held := read.check(doneByChecksCheck)
		switch {
		case !held || mainMergeCommit(block) != doneByChecksSecondMerge:
			return fmt.Errorf("%s's branch basis of main names merge_commit %q, want %s", doneByChecksCheck, mainMergeCommit(block), doneByChecksSecondMerge)
		case block.PassStands.Answer != dispatch.PassStandsNo || block.PassStands.Reason != dispatch.PassStandsReasonNoResult:
			return fmt.Errorf("%s answers %+v, want no for reason no_result: its pass does not hold the second merge", doneByChecksCheck, block.PassStands)
		}
		return nil
	})
	second := r.lookOf(tester, 1, stepRecordsWait)
	if check := readProductionCheck(t, r.st.Pool(), r.records, first); second.Equal(look.opened) || check.Generation != 2 || check.MergeSHA != doneByChecksSecondMerge {
		t.Fatalf("%s's production check after the second merge is %+v, want a check of generation 2 at %s", first, check, doneByChecksSecondMerge)
	}
	pass := r.record("pass", r.standIn.commitOnMain(doneByChecksSecondPassCommit))
	r.awaitResultFact(pass)
	reported := r.reportLook(tester, second, 1, "pass", nil)
	tester.endTurn()

	// Item 6: the issue closes on its checks.
	r.awaitCloseRead(first, "the second production check's pass", reported, passesAt(doneByChecksBeforeCheck, doneByChecksSecondHead))
	r.awaitIssue(first, "the close", func(issue api.Issue) error {
		if issue.Phase != phase.Done || issue.LingerUntil == nil {
			return fmt.Errorf("it stands in %s, lingering until %v, want done with its tree lingering", issue.Phase, issue.LingerUntil)
		}
		return nil
	})
	row := r.issueRow(first)
	if row.ClosedOnChecksAt == nil {
		t.Fatalf("%s closed with no closed_on_checks_at", first)
	}

	// Item 8: the wait was told in close-wait.md's versions, and in no message.
	versions := r.awaitCloseWaitDocument(first, "the close")
	for place := 1; place < len(versions); place++ {
		if versions[place] == versions[place-1] {
			t.Errorf("versions %d and %d of close-wait.md of %s are the same: a version that no change of the wait made", place, place+1, first)
		}
	}
	if last := versions[len(versions)-1]; !strings.Contains(last, closeWaitTime(*row.ClosedOnChecksAt)) {
		t.Errorf("the last version of close-wait.md of %s names no close at %s", first, closeWaitTime(*row.ClosedOnChecksAt))
	}
	if len(versions) < 3 {
		t.Errorf("close-wait.md of %s has %d versions, want one at the entry, one at the change's start and one at the close at least", first, len(versions))
	}
	if got := r.daemonMessages(first); got != look.messages {
		t.Errorf("%s holds %d messages in Dispatch at its close, want %d, as at its entry into verifying: a change of the wait posts none", first, got, look.messages)
	}
	t.Logf("close-wait.md of %s has %d versions from its entry to its close, and its messages stood at %d", first, len(versions), look.messages)

	// Item 7: a fail after the close tells the controller once and reopens nothing.
	lateFail := r.record("fail", r.standIn.commitOnMain(doneByChecksLateFailCommit))
	r.awaitResultFact(lateFail)
	var notices []listenerPublish
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the controller's closed-check-failed notice", func() bool {
		notices = r.controllerNotices(string(record.ClosedCheckFailedNotice))
		return len(notices) > 0
	})
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d closed-check-failed notices, want one: %+v", len(notices), notices)
	}
	t.Logf("the controller was told once: %s, %s", notices[0].Message, notices[0].Payload)
	r.awaitIssue(first, "the fail after the close", inPhase(phase.Done))
}

// thirdStaysOpenAfterItsMerge is the third issue, from the release before, from its sign-off to its
// merge: after it, it stands in verifying as the first did, and its close waits for the fifth
// issue beneath it.
func thirdStaysOpenAfterItsMerge(r *doneByChecksRig, third, fifth string) {
	r.t.Helper()
	r.on(third)
	r.walkSignOff()
	r.walkRetro(doneByChecksSprocketHead)
	merged := r.mergeOf(third, doneByChecksSprocketPull, doneByChecksSprocketMerge)
	r.awaitCloseRead(third, "the third issue's merge", merged, namesMerge(doneByChecksCheck, doneByChecksSprocketMerge))
	r.awaitIssue(third, "its merge", func(issue api.Issue) error {
		if err := inVerifying(issue); err != nil {
			return err
		}
		if issue.CloseWait == nil || !slices.ContainsFunc(issue.CloseWait.Beneath, func(beneath api.CloseWaitIssueView) bool { return beneath.Key == fifth }) {
			return fmt.Errorf("its closeWait %+v names no %s beneath it", issue.CloseWait, fifth)
		}
		return nil
	})
}

// c-before-merge-next-head (plan Task 11b, Step 2). An issue merges with its check before the merge
// passed at its pull request's head, and a fail of its check after the merge starts its next
// change. With that change's pull request open, Dispatch answers that the check before the merge
// has no pass that stands: its one pass is at the first pull request's head. The retro's gate reads
// that answer. After the change merges, with a pass at its own head, the close reads the check at
// the subject whose basis is the second pull request's final head.
func TestANextChangeNeedsAPassAtItsOwnHead(t *testing.T) {
	t.Parallel()
	r := newDoneByChecksRigWith(t, doneByChecksOptions{issues: []doneByChecksIssue{
		{title: doneByChecksTitle, verification: doneByChecksBothChecks, number: doneByChecksPull, head: stepRecordsHead},
	}})
	key, branch := r.key, claim.IssueBranch(r.key)
	r.walkPlanning()
	r.walkImplementing(stepRecordsHead)
	r.walkTesting()
	r.recordCheck(doneByChecksBeforeCheck, "pass", stepRecordsHead)
	r.walkReview(doneByChecksPull, stepRecordsHead)
	r.walkSignOff()
	r.walkRetro(stepRecordsHead)
	merged, status, body := r.mergeAndComplete(doneByChecksPull, doneByChecksMergeCommit)
	r.awaitCloseRead(key, "the first merge", merged, passesAt(doneByChecksBeforeCheck, stepRecordsHead))
	failed := r.awaitResultFact(r.record("fail", r.standIn.commitOnMain(doneByChecksLaterCommit)))
	r.awaitCloseRead(key, "the fail", failed, func(read doneByChecksRead) error {
		if block, held := read.check(doneByChecksCheck); !held || block.PassStands.Reason != dispatch.PassStandsReasonFail {
			return fmt.Errorf("%s answers %+v, want no for reason fail", doneByChecksCheck, block.PassStands)
		}
		return nil
	})
	r.awaitIssue(key, "the fail: the next change's start", func(issue api.Issue) error {
		if issue.Generation != 2 {
			return fmt.Errorf("it stands at generation %d in %s, want generation 2; the merger's completion after the merge answered %d %s",
				issue.Generation, issue.Phase, status, body)
		}
		return nil
	})
	r.walkPlanning()
	r.standIn.open(doneByChecksSecondPull, branch, doneByChecksSecondHead)
	opened := r.publishPull(doneByChecksSecondPull, "opened")
	r.awaitProcessed("github", opened)
	r.awaitAnswerOf(doneByChecksBeforeCheck, fmt.Sprintf("#%d opened", doneByChecksSecondPull), func(answer doneByChecksAnswer) error {
		if err := subjectNamed(doneByChecksSecondHead)(answer); err != nil {
			return err
		}
		if answer.PassStands.Answer != dispatch.PassStandsNo {
			return fmt.Errorf("pass_stands answers %q, want no: its one pass is at the first pull request's head %s", answer.PassStands.Answer, stepRecordsHead)
		}
		return nil
	})
	if t.Failed() {
		t.FailNow()
	}
	t.Logf("with #%d open, Dispatch answers no for %s, as the retro's gate reads it", doneByChecksSecondPull, doneByChecksBeforeCheck)
	r.walkImplementing(doneByChecksSecondHead)
	r.walkTesting()
	r.recordCheck(doneByChecksBeforeCheck, "pass", doneByChecksSecondHead)
	r.walkReview(doneByChecksSecondPull, doneByChecksSecondHead)
	r.walkSignOff()
	r.walkRetro(doneByChecksSecondHead)
	merged, _, _ = r.mergeAndComplete(doneByChecksSecondPull, doneByChecksSecondMerge)
	r.awaitCloseRead(key, "the second merge", merged, passesAt(doneByChecksBeforeCheck, doneByChecksSecondHead))
	r.awaitIssue(key, "the second merge", func(issue api.Issue) error {
		if issue.CloseWait == nil || issue.Phase != phase.Verifying {
			return fmt.Errorf("it stands in %s with closeWait %+v, want verifying", issue.Phase, issue.CloseWait)
		}
		if slices.ContainsFunc(issue.CloseWait.Checks, func(check api.CloseWaitCheckView) bool { return check.ID == doneByChecksBeforeCheck }) {
			return fmt.Errorf("its close waits for %s, which passes at the second pull request's final head %s", doneByChecksBeforeCheck, doneByChecksSecondHead)
		}
		return nil
	})
}

// A write of its own issue while the issue waits (D17, ruling 4): an issue in verifying waits for a
// check after the merge whose evidence is an edit of the issue's own document. A role's edit of
// that document is taken by Dispatch, the check's result is recorded as a pass, and the issue
// closes at the read after it. The same edit once the issue closed is refused ISSUE_CLOSED, which
// is what the release before met, since it closed the issue at its merge.
func TestAWriteToItsOwnIssueIsTakenWhileTheIssueWaits(t *testing.T) {
	t.Parallel()
	r := newDoneByChecksRigWith(t, doneByChecksOptions{issues: []doneByChecksIssue{
		{title: doneByChecksTitle, verification: doneByChecksOpenItemsVerification, documents: map[string]string{doneByChecksOpenItems: doneByChecksOpenItemsBefore},
			number: doneByChecksPull, head: stepRecordsHead},
	}})
	r.walkToMerging(doneByChecksPull, stepRecordsHead)
	merged := r.mergeOf(r.key, doneByChecksPull, doneByChecksMergeCommit)
	r.awaitCloseRead(r.key, "the merge", merged, namesMerge(doneByChecksOpenItemsCheck, doneByChecksMergeCommit))
	r.awaitIssue(r.key, "the merge", inVerifying)
	implementer := r.worker(claim.RoleImplementer)
	editOpenItems(r, implementer)
	closesOnPassOf(r, doneByChecksOpenItemsCheck, doneByChecksMergeCommit)
	openItemsRefused(r, implementer)
}

// editOpenItems is c's edit of the issue's open items, which Dispatch must take while the issue
// waits: its new version in Dispatch is the barrier.
func editOpenItems(r *doneByChecksRig, c *stepClaimant) {
	t := r.t
	t.Helper()
	if status, body := r.editDocument(r.key, c, doneByChecksOpenItems, doneByChecksOpenItemsAfter); status != http.StatusCreated {
		t.Fatalf("%s's edit of %s of %s = %d %s, want 201: the issue waits in verifying", c.token, doneByChecksOpenItems, r.key, status, body)
	}
	var versions int
	testwait.WithinPolling(t, stepRecordsWait, 200*time.Millisecond, "the edit's version of "+doneByChecksOpenItems, func() bool {
		versions = r.documentVersions(r.key, strings.ReplaceAll(doneByChecksOpenItems, ".", "-"))
		return versions == 2
	})
	t.Logf("Dispatch took %s's edit of %s of %s: version %d", c.token, doneByChecksOpenItems, r.key, versions)
}

// closesOnPassOf records a pass of check at commit, and the read after it closes the walk's issue,
// both in the daemon and in Dispatch.
func closesOnPassOf(r *doneByChecksRig, check, commit string) {
	t := r.t
	t.Helper()
	passed := r.awaitResultFact(r.recordCheck(check, "pass", commit))
	r.awaitCloseRead(r.key, "the pass of "+check, passed, func(read doneByChecksRead) error {
		if block, held := read.check(check); !held || block.PassStands.Answer != dispatch.PassStandsYes {
			return fmt.Errorf("%s answers %+v, want yes", check, block.PassStands)
		}
		return nil
	})
	r.awaitIssue(r.key, "the read after the pass", inPhase(phase.Done))
	var status struct {
		Status string `json:"status"`
	}
	testwait.WithinPolling(t, stepRecordsWait, 200*time.Millisecond, "Dispatch's status of "+r.key+" done", func() bool {
		return r.personRead("/api/v1/issues/"+r.key, &status) == nil && status.Status == record.DoneStatus
	})
	t.Logf("%s closed at the read after the pass of %s: Dispatch shows it %s", r.key, check, status.Status)
}

// openItemsRefused is c's same edit once the issue closed: Dispatch refuses it ISSUE_CLOSED.
func openItemsRefused(r *doneByChecksRig, c *stepClaimant) {
	t := r.t
	t.Helper()
	status, body := r.editDocument(r.key, c, doneByChecksOpenItems, doneByChecksOpenItemsAfter+"- ACME-8: merged as acme/widgets#8\n")
	if status != http.StatusConflict || !strings.Contains(string(body), `"ISSUE_CLOSED"`) {
		t.Fatalf("%s's edit of %s of the closed %s = %d %s, want 409 ISSUE_CLOSED", c.token, doneByChecksOpenItems, r.key, status, body)
	}
	t.Logf("the same edit on the closed %s: %d %s", r.key, status, body)
}

// c-children-read (plan Task 11b, Step 3). After the daemon's boot, the architect files a child of
// the merged root in backlog in Dispatch. The root's check passes, and the root stays in verifying,
// close-wait.md naming the child, until the child moves to icebox: the controller's tick reads the
// root's children from Dispatch again, and the root closes at that read.
func TestAChildFiledAfterTheBootHoldsItsRootsClose(t *testing.T) {
	t.Parallel()
	r := newDoneByChecksRigWith(t, doneByChecksOptions{controllerWake: 2 * time.Second, issues: []doneByChecksIssue{
		{title: doneByChecksTitle, verification: doneByChecksVerification, number: doneByChecksPull, head: stepRecordsHead},
	}})
	root := r.key
	r.walkToMerging(doneByChecksPull, stepRecordsHead)
	merged := r.mergeOf(root, doneByChecksPull, doneByChecksMergeCommit)
	r.awaitCloseRead(root, "the merge", merged, namesMerge(doneByChecksCheck, doneByChecksMergeCommit))
	r.awaitIssue(root, "the merge", inVerifying)
	architect := r.worker(claim.RoleArchitect)
	child := r.fileChild(architect, "Bolts tighten after the merge")
	passed := r.awaitResultFact(r.record("pass", doneByChecksMergeCommit))
	r.awaitCloseRead(root, "the pass with the child filed", passed, func(read doneByChecksRead) error {
		block, held := read.check(doneByChecksCheck)
		if !held || block.PassStands.Answer != dispatch.PassStandsYes {
			return fmt.Errorf("%s answers %+v, want yes", doneByChecksCheck, block.PassStands)
		}
		if filed, listed := read.child(child); !listed || filed.Status != "backlog" {
			return fmt.Errorf("the read's children name %s as %+v, want it in backlog", child, filed)
		}
		return nil
	})
	r.awaitIssue(root, "the child in backlog", func(issue api.Issue) error {
		if err := inVerifying(issue); err != nil {
			return err
		}
		if !slices.ContainsFunc(issue.CloseWait.OpenChildren, func(open api.CloseWaitIssueView) bool { return open.Key == child }) {
			return fmt.Errorf("its closeWait %+v names no open child %s", issue.CloseWait, child)
		}
		return nil
	})
	versions := r.awaitCloseWaitDocument(root, "the child in backlog")
	if !strings.Contains(versions[len(versions)-1], child) {
		t.Fatalf("close-wait.md of %s names no %s", root, child)
	}
	r.sessionStatus(child, architect, "icebox")
	moved := time.Now()
	r.awaitCloseRead(root, "the child's move to icebox", moved, func(read doneByChecksRead) error {
		if filed, listed := read.child(child); listed && filed.Status != "icebox" {
			return fmt.Errorf("the read's children name %s as %+v, want it in icebox", child, filed)
		}
		return nil
	})
	r.awaitIssue(root, "the child's move to icebox", inPhase(phase.Done))
}

// fileChild is c's session filing a child of the walk's issue in Dispatch, as a role's `dispatch
// create` files one, and moving it to backlog, where Dispatch files it elsewhere. It answers the
// child's key.
func (r *doneByChecksRig) fileChild(c *stepClaimant, title string) string {
	t := r.t
	t.Helper()
	code, body := r.agent(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": r.cfg.Project, "title": title, "parent": r.key, "actor": map[string]string{"kind": "session", "id": c.session},
	})
	if code != http.StatusCreated {
		t.Fatalf("%s's child of %s = %d %s, want 201", c.token, r.key, code, body)
	}
	var filed struct {
		Key    string `json:"key"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &filed); err != nil || filed.Key == "" {
		t.Fatalf("decode %s's child of %s %s: %v", c.token, r.key, body, err)
	}
	if filed.Status != "backlog" {
		r.sessionStatus(filed.Key, c, "backlog")
	}
	t.Logf("%s filed %s under %s after the boot, in backlog", c.token, filed.Key, r.key)
	return filed.Key
}

// documentVersions is how many versions issue's document of slug holds in Dispatch, 0 for none.
func (r *doneByChecksRig) documentVersions(issue, slug string) int {
	r.t.Helper()
	var artifacts []struct {
		Slug     string            `json:"slug"`
		Versions []json.RawMessage `json:"versions"`
	}
	if err := r.personRead("/api/v1/issues/"+issue+"/artifacts", &artifacts); err != nil {
		r.t.Fatalf("read the documents of %s: %v", issue, err)
	}
	for _, artifact := range artifacts {
		if artifact.Slug == slug {
			return len(artifact.Versions)
		}
	}
	return 0
}

// namesMerge is a read that holds the merge: check's branch basis of main names merge as its
// merge_commit.
func namesMerge(check, merge string) func(doneByChecksRead) error {
	return func(read doneByChecksRead) error {
		block, held := read.check(check)
		if !held {
			return fmt.Errorf("the read holds no check %s", check)
		}
		if got := mainMergeCommit(block); got != merge {
			return fmt.Errorf("%s's branch basis of main names merge_commit %q, want %s", check, got, merge)
		}
		return nil
	}
}

// passesAt is a read whose before_merge check passes at the subject whose basis is head.
func passesAt(check, head string) func(doneByChecksRead) error {
	return func(read doneByChecksRead) error {
		block, held := read.check(check)
		if !held {
			return fmt.Errorf("the read holds no check %s", check)
		}
		if subject, named := subjectAt(block, head); !named || subject.Answer != dispatch.PassStandsYes {
			return fmt.Errorf("%s's subject at %s is %+v (named %t), want a pass there", check, head, subject, named)
		}
		return nil
	}
}

// subjectNamed is Dispatch's answer of a before_merge check whose subjects name head.
func subjectNamed(head string) func(doneByChecksAnswer) error {
	return func(answer doneByChecksAnswer) error {
		for _, subject := range answer.PassStands.Subjects {
			if subject.Basis.Commit == head {
				return nil
			}
		}
		return fmt.Errorf("pass_stands names no subject at %s", head)
	}
}

// inPhase is an issue the state shows in want.
func inPhase(want phase.Phase) func(api.Issue) error {
	return func(issue api.Issue) error {
		if issue.Phase != want {
			return fmt.Errorf("it stands in %s, want %s", issue.Phase, want)
		}
		return nil
	}
}

// inVerifying is an issue the state shows in verifying with the status testing, and what its close
// waits for.
func inVerifying(issue api.Issue) error {
	if issue.Phase != phase.Verifying || issue.Status != "testing" || issue.CloseWait == nil {
		return fmt.Errorf("it stands in %s with status %s and closeWait %+v, want verifying with status testing", issue.Phase, issue.Status, issue.CloseWait)
	}
	return nil
}

// productionOf is the production check the state shows an issue's close waiting for, nil for none.
func productionOf(issue api.Issue) *api.CloseWaitProductionView {
	if issue.CloseWait == nil {
		return nil
	}
	return issue.CloseWait.Production
}

// closeWaitTime is at as close-wait.md writes a moment: UTC, to the second.
func closeWaitTime(at time.Time) string {
	return at.UTC().Format("2006-01-02 15:04:05 UTC")
}
