package workflow

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// wakeRecordAt records the wake rig's pull request at head, as newWakeRig records it at head1:
// open, with no settlement and no head run.
func wakeRecordAt(t *testing.T, rig wakeRig, head string) {
	t.Helper()
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, Failing: []string{}})
}

// noWake fails the test unless the outbox holds no start row and LEGION-208 no resume, after what.
func noWake(t *testing.T, rig wakeRig, what string) {
	t.Helper()
	if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
		t.Fatalf("after %s the supervise start rows = %+v, want none", what, starts)
	}
	if resumes := wakeResumes(t, rig.pool); len(resumes) != 0 {
		t.Fatalf("after %s the pod resumes = %v, want none", what, resumes)
	}
}

// A settlement wakes a released worker only where it is the settlement checks applies for the
// record's head (dispatch://INFRA-4135 R4), on the wake rig (LEGION-208 implementing, its pull
// request #42 at head1, its implementer released), in each way the record comes to stand at head1:
// as the rig records it, from head0 by its push and its synchronize, and from head0 by a
// synchronize whose push has not arrived, which opens a head run and already makes head1 the
// record's head, so a settlement of head1 stands on arrival:
//
//   - a settlement of head0, an old head, queues no start row and records no resume;
//   - a settlement of head1 then wakes the implementer once;
//   - with the pod back and released again after that wake (its start run, a new release, the
//     claim released once more), the same settlement delivered again wakes nobody, since it
//     applies nothing;
//   - and a settlement of head1 at a later generation then wakes the implementer a second time,
//     so the release did let a wake through and the redelivery was turned away by the rule alone.
//
// Negative control: at ac57d598 the wake reads no head, so the settlement of head0 wakes the
// implementer with a task naming head0 the head, and the redelivery after the second release
// wakes it a second time.
func TestWakeOnSettlementOfAnOldHeadWakesNobody(t *testing.T) {
	const dev = "legion-implementer[bot]"
	for _, tc := range []struct {
		name string
		// from is the head the record is put at before the facts of before, "" for the rig's own
		// head1.
		from    string
		before  []intake.Fact
		runOpen bool
	}{
		{name: "head1 as the rig records it"},
		{name: "head1 reached from head0 by its push and its synchronize", from: "head0",
			before: []intake.Fact{sentPush("head0", "head1", dev), namedSync("head0", "head1", dev, wakeAt.Add(-time.Hour))}},
		{name: "head1 reached from head0 by a synchronize whose push has not arrived", from: "head0", runOpen: true,
			before: []intake.Fact{namedSync("head0", "head1", dev, wakeAt.Add(-time.Hour))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			if tc.from != "" {
				wakeRecordAt(t, rig, tc.from)
			}
			for i, fact := range tc.before {
				rig.apply(fmt.Sprintf("before-%d", i), fact)
			}
			if pr := probePullRequest(t, rig.pool); pr.HeadSHA != "head1" || (pr.Run != nil) != tc.runOpen {
				t.Fatalf("before the settlements the pull request is at %s with a head run open %t, want head1 and %t", pr.HeadSHA, pr.Run != nil, tc.runOpen)
			}
			noWake(t, rig, "the facts before the settlements")

			rig.apply("old-head", settlement("head0", "build"))
			noWake(t, rig, "a settlement of head0, which the record at head1 does not hold")

			rig.apply("head", settlement("head1", "build"))
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1", "build")

			// The pod comes back and is released again. The first wake's start is what brings it
			// back, and the outbox deletes that start once it has run it (outboxRan), so the pod is
			// back only after its start ran. The test runs it here because a wake of a role whose
			// start is still queued takes nothing (wakeReleased, Pods.StartQueued), a gate this
			// pull request added, which main's wake did not read. Left queued, that start would
			// turn the later settlement away, and it would turn the redelivery away whatever the
			// rule says. ran is that start, which the assertions below count with the ones the
			// outbox still holds. Then a release that suspends the implementer, recorded after
			// the wake's resume, which the next wake's take closes as the resume of a pod gone
			// away (wakeReleased), and the claim released once more.
			ran := wakeStarts(t, rig.pool)
			outboxRan(t, rig.pool)
			seedRelease(t, rig.pool, "implementer", wakeAt.Add(time.Minute))
			claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "released")
			rig.apply("head-again", settlement("head1", "build"))
			if starts := wakeStarts(t, rig.pool); len(starts) != 0 {
				t.Fatalf("supervise start rows after the settlement of head1 delivered again = %+v, want none: a redelivery applies nothing", starts)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 1 || resumes[0] != [2]string{string(claim.RoleImplementer), pods.WakeChecks} {
				t.Fatalf("pod resumes after the settlement of head1 delivered again = %v, want the first wake's alone", resumes)
			}

			rig.apply("head-later", laterSettlement("head1", 2))
			if starts := append(ran, wakeStarts(t, rig.pool)...); len(starts) != 2 || starts[1].Role != claim.RoleImplementer || !strings.Contains(starts[1].Task, "head1") {
				t.Fatalf("supervise start rows after a later settlement of head1 = %+v, want the first wake's and a second of the implementer naming head1", starts)
			}
			if resumes := wakeResumes(t, rig.pool); len(resumes) != 2 || resumes[1] != [2]string{string(claim.RoleImplementer), pods.WakeChecks} {
				t.Fatalf("pod resumes after a later settlement of head1 = %v, want the first wake's and a second %s wake of the implementer", resumes, pods.WakeChecks)
			}
		})
	}
}

// A settlement of a head the record has not reached yet, one that arrives before that head's
// synchronize, stands for no head the record holds, so it wakes nobody (dispatch://INFRA-4135 R4),
// on the wake rig with the implementer released and the record at head0, in each way it stands
// there: with no head run open, and reached by a synchronize whose push has not arrived, so a head
// run is open. The settlement of head1 queues no start row, records no resume and tells no role,
// and checks' drop line names it as a settlement that stands for no head the record holds, with
// the recorded head head0 and whether a head run is open (R6). Applying it once its head arrives
// is dispatch://INFRA-4665's R3, not this rule's.
//
// The control is a settlement of head0, the head the record holds, which then wakes the
// implementer once.
//
// Negative control: at ac57d598 the wake reads no head, so the settlement of head1 wakes the
// implementer, and checks writes no drop line.
func TestWakeOnSettlementOfAnEarlyHeadWakesNobody(t *testing.T) {
	const dev = "legion-implementer[bot]"
	// priorHead is the head the record held before head0's synchronize arrived.
	const priorHead = "head-prior"
	for _, tc := range []struct {
		name string
		// from is the head the record is put at before the facts of before.
		from    string
		before  []intake.Fact
		runOpen bool
	}{
		{name: "the record at head0 with no head run open", from: "head0"},
		{name: "the record at head0 reached by a synchronize whose push has not arrived", from: priorHead, runOpen: true,
			before: []intake.Fact{namedSync(priorHead, "head0", dev, wakeAt.Add(-time.Hour))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newWakeRig(t)
			wakeRecordAt(t, rig, tc.from)
			for i, fact := range tc.before {
				rig.apply(fmt.Sprintf("before-%d", i), fact)
			}
			if pr := probePullRequest(t, rig.pool); pr.HeadSHA != "head0" || (pr.Run != nil) != tc.runOpen {
				t.Fatalf("before the settlement the pull request is at %s with a head run open %t, want head0 and %t", pr.HeadSHA, pr.Run != nil, tc.runOpen)
			}
			noWake(t, rig, "the facts before the settlement")
			if lines := droppedSettlementLines(rig.logged.String()); len(lines) != 0 {
				t.Fatalf("the facts before the settlement logged %q, want no drop", lines)
			}
			told := roleMessageCount(t, rig.pool)

			rig.apply("early-head", settlement("head1", "build"))

			noWake(t, rig, "a settlement of head1, whose synchronize has not arrived")
			if got := roleMessageCount(t, rig.pool); got != told {
				t.Fatalf("the settlement of head1 queued %d role messages, want none", got-told)
			}
			lines := droppedSettlementLines(rig.logged.String())
			if len(lines) != 1 {
				t.Fatalf("logged %d drop lines %q, want one", len(lines), lines)
			}
			if !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], `msg="`+standsForNoHeadLine+`"`) {
				t.Fatalf("the drop line is %q, want %q at INFO", lines[0], standsForNoHeadLine)
			}
			for _, name := range []string{" number=42", " head=head1", " generation=1", " snapshot=head1-snapshot", " recorded_head=head0",
				fmt.Sprintf(" head_run_open=%t", tc.runOpen), " issue=LEGION-208"} {
				if !strings.Contains(lines[0], name) {
					t.Fatalf("the drop line %q does not name %q", lines[0], name)
				}
			}

			rig.apply("head", settlement("head0", "build"))
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head0", "build")
		})
	}
}
