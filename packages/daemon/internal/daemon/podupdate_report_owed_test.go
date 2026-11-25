package daemon

// The round's early end waits for an owed report (row 39k of dispatch://INFRA-2811's plan): a
// claim whose take's turn ended with the report naming the task's number still to come holds the
// round's early end, beside a claim stored working, since a pod replaced between that turn's end
// and its report takes the report with it and the relaunched process runs the task again. The
// claim is read from its machine's published view (supervise.Claim.ReportOwed) and kept in a
// reading of its own (reportingClaims), not among the turns that run. These cases drive the
// pod-update rig of podupdate_test.go, with the bound of podupdate_bound_test.go.

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reportOwedWaitLine is the line the walk logs for a round an owed report keeps open before its
// bound (completePodUpdate). It names the issue, the round and the roles whose report is owed
// (reporting), and it is written only once every asked claim has answered and no turn runs.
const reportOwedWaitLine = "supervise: every worker of this update round has answered and no turn runs; the replacement waits for the report of a take whose turn has ended, or for the round's bound"

// reportOwedHarnesses is each harness a worker of the rig runs on: Oh My Pi, the rig's own, and
// Claude Code, a backend whose launch installs no plugin (noPluginInstall).
var reportOwedHarnesses = []struct {
	name    string
	backend runtime.Backend
}{
	{name: "Oh My Pi"},
	{name: "Claude Code", backend: runtime.BackendClaude},
}

// reportOwedRound is a rig whose implementer, on backend where one is named, took task number,
// answered the round from inside that take's turn, and ended the turn with the turn-outcome
// report naming the number still to come; its tester answered too, and each reported that nothing
// of its own runs since its last turn started. Every claim the round asks has answered, and no
// turn runs.
func reportOwedRound(t *testing.T, backend runtime.Backend) (rig *podUpdateRig, number uint64) {
	t.Helper()
	worked := claim.RoleImplementer
	rig = newPodUpdateRig(t, readyWorker(worked), readyWorker(claim.RoleTester))
	if backend != "" {
		rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{worked: backend}
	}
	token := rig.tokens[worked]
	rig.handle(worked, supervise.RequestDeliver{Claim: token, Task: "Continue Workflow. Issue: LEGION-484. Phase: implementing.",
		ID: "outbox:39", Phase: phase.Implementing, Generation: 1})
	if answer := rig.take(worked); answer.Outcome != supervise.TakeOutcomeAccepted {
		t.Fatalf("the implementer's take of its task = %+v, want it accepted", answer)
	}
	number = rig.claimOf(worked).Pending.Number
	rig.handle(worked, supervise.StreamTurnStart{Claim: token})

	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(worked, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.clockAt(podUpdateStart.Add(time.Minute))
	rig.answersWithoutReport(worked, "a1b2c3d")
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.handle(worked, supervise.StreamTurnEnd{Claim: token})
	rig.reportsNothingRunning(worked)

	c := rig.settledClaim(worked)
	if c.State != supervise.StateIdle || c.Pending == nil || c.Pending.Number != number || c.Pending.ConfirmedAt.IsZero() || !c.ReportOwed {
		t.Fatalf("the implementer is %s holding %+v with its report owed %t, want it idle over task %d's take, its report owed",
			c.State, c.Pending, c.ReportOwed, number)
	}
	return rig, number
}

// reportOwedHeldBeforeTheBound reads the round's end over the claims as their machines hold them
// now, and holds that it names the implementer in reporting and no turn, and that the round would
// still read its workspace (settled); then it sweeps the round at its opening's minute, half-way to
// its bound and a minute before it, and holds that each sweep replaced nothing and logged the
// round as waiting for the implementer's report, naming no turn.
func reportOwedHeldBeforeTheBound(t *testing.T, rig *podUpdateRig) {
	t.Helper()
	rig.settleAll()
	told, live := rig.s.liveGroup(rig.snapshot())
	clock := roundClock{now: rig.clock.Now(), grace: podUpdateGrace}
	_, working, reporting, _ := readRoundEnd(live, podUpdateRound, clock).holding()
	if !slices.Equal(reporting, []string{string(claim.RoleImplementer)}) || len(working) != 0 {
		t.Fatalf("the round's end holds the replacement for the reports of %v and the turns of %v, want the implementer's report and no turn", reporting, working)
	}
	if !readRoundEnd(told, podUpdateRound, clock).settled() {
		t.Fatal("the round's end reads unsettled with the implementer's report owed, want the read of the workspace still made: a worker whose turn is over changes no file of the working copy")
	}
	for _, at := range []time.Duration{time.Minute, podUpdateGrace / 2, podUpdateGrace - time.Minute} {
		rig.clockAt(podUpdateStart.Add(at))
		rig.sweepSettled()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, before the bound %s, the pod was replaced %d times while the implementer's report of its task's turn was owed",
				rig.clock.Now(), podUpdateBound, len(calls))
		}
	}
	lines := loggedLines(t, rig.logs.String(), reportOwedWaitLine)
	if len(lines) == 0 {
		t.Fatal("the sweep logged no line of the owed report, want each sweep to name the implementer")
	}
	for _, line := range lines {
		if line["issue"] != podUpdateIssue || line["round"] != podUpdateRound {
			t.Errorf("the owed report's line is %+v, want it of issue %s and round %s", line, podUpdateIssue, podUpdateRound)
		}
		wantField(t, line, "reporting", claim.RoleImplementer)
		if _, present := line["working"]; present {
			t.Errorf("the owed report's line names working %v, want no turn named: the implementer's turn is over", line["working"])
		}
	}
}

// A round whose every asked claim has answered and in which no turn runs does not end early while
// the report of the implementer's task's turn is owed: its turn ended, and the report naming the
// task's number has not come. Before the bound each sweep replaces nothing and names the
// implementer as a report owed, and no turn. Then one of two ends:
//
//   - The report comes and finishes the task: the next sweep replaces the pod before the bound,
//     as for a round its answers end, and no line says the bound made the replacement.
//   - The round's deadline, which stays the hard bound (roundClock.pastBound), comes with the
//     report still owed: the pod is replaced there, and the bound's own line names the implementer
//     among the roles whose report was owed (reporting), apart from the turns that ran and the
//     claims that had not answered, of which there were none. A pod deleted at the deadline with
//     the report owed is decision 8's accepted case, as a turn the round cuts is.
//
// Each holds for a Claude Code claim as for an Oh My Pi one.
//
// The negative control is the head this test was written at, where the round's early end reads
// only the turns that run (workingClaims): the first sweep after the turn's end replaces the pod
// with the report still owed.
func TestPodUpdateRoundWaitsForTheReportOfATaskNumberWhoseTurnEnded(t *testing.T) {
	t.Parallel()
	for _, harness := range reportOwedHarnesses {
		t.Run(harness.name+": the report ends the round early", func(t *testing.T) {
			t.Parallel()
			rig, number := reportOwedRound(t, harness.backend)
			clocks := rig.replacementClocks()
			reportOwedHeldBeforeTheBound(t, rig)

			reported := rig.claimOf(claim.RoleImplementer)
			rig.handle(claim.RoleImplementer, supervise.RequestTurnOutcome{Claim: reported.Token, Generation: reported.Generation,
				Sequence: turnSequence.Add(1), Task: number})
			if c := rig.settledClaim(claim.RoleImplementer); c.Pending != nil || c.TaskFinished != number || c.ReportOwed {
				t.Fatalf("the implementer holds %+v with task %d finished and its report owed %t, want task %d finished by its report",
					c.Pending, c.TaskFinished, c.ReportOwed, number)
			}
			rig.awaitReplacement(1, "once the report finished the task")
			for _, at := range clocks() {
				if !at.Before(podUpdateBound) {
					t.Fatalf("the replacement was made at %s, want it before the bound %s: the report came", at, podUpdateBound)
				}
			}
			if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != false {
				t.Fatalf("the replacement was logged %+v, want one line that no bound made", replaced)
			}
			if lines := loggedLines(t, rig.logs.String(), podUpdateBoundLine); len(lines) != 0 {
				t.Fatalf("the sweep logged a bounded end %+v for a round that ended once the report came", lines)
			}
		})
		t.Run(harness.name+": the bound ends the round over the owed report", func(t *testing.T) {
			t.Parallel()
			rig, number := reportOwedRound(t, harness.backend)
			clocks := rig.replacementClocks()
			reportOwedHeldBeforeTheBound(t, rig)

			rig.clockAt(podUpdateBound)
			rig.awaitReplacement(1, "at the bound, with the implementer's report still owed")
			for _, at := range clocks() {
				if at.Before(podUpdateBound) {
					t.Fatalf("a replacement was made at %s, before the bound %s", at, podUpdateBound)
				}
			}
			if replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine); len(replaced) != 1 || replaced[0]["atTheBound"] != true {
				t.Fatalf("the replacement was logged %+v, want one line that says the bound made it", replaced)
			}
			bounded := loggedLines(t, rig.logs.String(), podUpdateBoundLine)
			if len(bounded) != 1 {
				t.Fatalf("the bounded end was logged %d times, want once: %+v", len(bounded), bounded)
			}
			line := bounded[0]
			if line["round"] != podUpdateRound || line["pod"] != podUpdatePod || line["issue"] != podUpdateIssue {
				t.Errorf("the bounded end's line is %+v, want it of round %s, pod %s and issue %s", line, podUpdateRound, podUpdatePod, podUpdateIssue)
			}
			wantField(t, line, "reporting", claim.RoleImplementer)
			wantField(t, line, "working")
			wantField(t, line, "unanswered")
			wantField(t, line, "answered", claim.RoleImplementer, claim.RoleTester)
			if p := rig.claimOf(claim.RoleImplementer).Pending; p == nil || p.Number != number {
				t.Fatalf("the implementer holds %+v at the bound, want task %d still waiting for its report", p, number)
			}
		})
	}
}

// A claim stored working can carry a report owed too: a compaction with no turn under it runs over
// a take whose turn ended with its report still to come. The round's end names such a claim among
// the turns that run (workingClaims) and not among the reports owed (reportingClaims), so it is
// named once, and the wait for its work is what holds the round for it.
func TestPodUpdateRoundNamesAWorkingClaimWithItsReportOwedAmongTheTurnsAlone(t *testing.T) {
	t.Parallel()
	opened := podUpdateStart
	c := supervise.Claim{Token: mustClaimToken(t, podUpdateIssue, claim.RoleImplementer), Project: podUpdateProject,
		Tree: podUpdateIssue, Issue: podUpdateIssue, Role: claim.RoleImplementer, State: supervise.StateWorking,
		Session: "ses-implementer", ReportOwed: true,
		UpdateRound: podUpdateRound, UpdateReasons: podUpdateDrift, UpdateOpenedAt: &opened}
	clock := roundClock{now: opened.Add(time.Minute), grace: podUpdateGrace}
	_, working, reporting, _ := readRoundEnd([]supervise.Claim{c}, podUpdateRound, clock).holding()
	if !slices.Equal(working, []string{string(claim.RoleImplementer)}) || len(reporting) != 0 {
		t.Fatalf("the round's end holds the replacement for the turns of %v and the reports of %v, want the implementer's turn alone", working, reporting)
	}
}
