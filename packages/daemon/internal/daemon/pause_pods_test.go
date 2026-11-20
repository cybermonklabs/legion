package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// factCollector is a handler that keeps every fact it is handed.
type factCollector struct{ facts []intake.Fact }

func (c *factCollector) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	c.facts = append(c.facts, fact)
	return intake.Result{}, nil
}

// Decision 22 of dispatch://INFRA-3759: the resume's one admission pass is a fact of its own,
// intake.ProjectResumed, applied through the workflow's handlers and keyed on the ended pause, so
// a resume applies it once and the resume of another pause applies its own. It is no controller
// tick, which would queue a tick notice (internal/admit's
// TestTheResumesPassPromotesAndQueuesNoTickNotice holds what admission does with it).
func TestTheResumesPassIsAFactOfItsOwnAppliedOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	collector := &factCollector{}
	p := &projectPause{project: "LEGION", log: quietLogger(),
		workflow: &workflowRuntime{pool: pool, handlers: []intake.Handler{collector}, log: quietLogger()}}

	p.applyResumePass(context.Background(), store.Pause{ID: 7})
	p.applyResumePass(context.Background(), store.Pause{ID: 7})
	p.applyResumePass(context.Background(), store.Pause{ID: 8})

	if len(collector.facts) != 2 || collector.facts[0] != (intake.ProjectResumed{}) || collector.facts[1] != (intake.ProjectResumed{}) {
		t.Fatalf("facts applied = %#v, want one ProjectResumed for pause 7 and one for pause 8", collector.facts)
	}
}

// openLegionPause opens a pause of project LEGION at at, as the operator's pause writes its row
// (store's Pause, project_pauses), and answers its id.
func openLegionPause(t *testing.T, pool *store.Pool, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into project_pauses (project, began_at, by_kind, reason)
		values ('LEGION', $1, 'operator', 'the model provider is down') returning id`, at).Scan(&id); err != nil {
		t.Fatalf("pause LEGION: %v", err)
	}
	return id
}

// endLegionPause ends pause id at at, as the operator's resume ends it (store's Resume).
func endLegionPause(t *testing.T, pool *store.Pool, id int64, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update project_pauses
		set ended_at = $2, ended_by_kind = 'operator', ended_by_session = '' where id = $1`, id, at); err != nil {
		t.Fatalf("resume LEGION: %v", err)
	}
}

// holdLegionPod opens pause's hold of issue's pod at at, in a transaction of its own and under the
// issue's lock, as a stop transaction opens one (store.LockIssueForPause,
// store.OpenPausePodHoldTx). pause is a pause of LEGION (openLegionPause).
func holdLegionPod(t *testing.T, st *store.Store, pause int64, issue string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		if err := store.LockIssueForPause(ctx, tx, issue); err != nil {
			return err
		}
		_, err := store.OpenPausePodHoldTx(ctx, tx, store.Pause{ID: pause, Project: "LEGION"}, issue, at)
		return err
	}); err != nil {
		t.Fatalf("hold the pod of %s: %v", issue, err)
	}
}

// podHoldEnd is how issue's newest pod hold ended, "" while it stands.
func podHoldEnd(t *testing.T, pool *store.Pool, issue string) string {
	t.Helper()
	var end string
	if err := pool.QueryRow(context.Background(), `select coalesce(ended_why, '') from project_pause_pod_holds
		where issue = $1 order by id desc limit 1`, issue).Scan(&end); err != nil {
		t.Fatalf("read the pod hold of %s: %v", issue, err)
	}
	return end
}

// Decision 14 of dispatch://INFRA-3759: the stuck warning measures a resume from its stuck clock,
// which the wake pacing's let-out moved past the wake's own moment, and passes over every resume
// of a project paused now. Ten minutes after the wake, with the clock younger than that, nothing
// is stuck; past the clock's ten minutes, a paused project is warned of nothing; once the pause
// ends, the same pass warns. A resume of a claim of another project's issue, which this daemon
// runs under its own project and the pause holds, is passed over the same way: the pause is the
// daemon's project's, not its issue key's.
func TestTheStuckWarningReadsTheStuckClockAndPassesOverAPausedProject(t *testing.T) {
	w, pool, logged := wakeRuntime(t)
	ctx := context.Background()
	woke := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	letOut := woke.Add(30 * time.Minute)
	for _, issue := range []string{"LEGION-208", "OTHERPROJECT-5"} {
		recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: issue,
			Role: string(claim.RoleImplementer), Kind: pods.WakeBackstop, At: woke})
		if _, err := w.idleStore.SetResumeClock(ctx, "legion", issue, claim.RoleImplementer, letOut); err != nil {
			t.Fatalf("start the stuck clock of %s at the let-out: %v", issue, err)
		}
	}
	const warning = "has had no first turn"

	w.warnStuckResumes(ctx, woke.Add(workflow.StrandedReleaseWake+time.Minute))
	if strings.Contains(logged.String(), warning) {
		t.Fatalf("the log = %q, want no warning: the wake's own ten minutes are not its stuck clock's", logged)
	}

	pause := openLegionPause(t, pool, letOut)
	w.warnStuckResumes(ctx, letOut.Add(workflow.StrandedReleaseWake))
	if strings.Contains(logged.String(), warning) {
		t.Fatalf("the log = %q, want no warning while the project is paused, for its own issue or another project's", logged)
	}

	endLegionPause(t, pool, pause, letOut.Add(time.Minute))
	w.warnStuckResumes(ctx, letOut.Add(workflow.StrandedReleaseWake))
	for _, issue := range []string{"LEGION-208", "OTHERPROJECT-5"} {
		if !strings.Contains(logged.String(), "issue="+issue) {
			t.Fatalf("the log = %q, want a warning for %s once the pause has ended", logged, issue)
		}
	}
	if !strings.Contains(logged.String(), warning) || !strings.Contains(logged.String(), "clock="+letOut.Format(time.RFC3339)) {
		t.Fatalf("the log = %q, want warnings naming the stuck clock %s", logged, letOut.Format(time.RFC3339))
	}
}

// Decision 13 of dispatch://INFRA-3759: a pod's hold ends at the first ready of a role of the pod,
// which reaches the daemon as the claim's durable ready (podResume).
func TestAPodHoldEndsAtTheReadyOfARoleOfThePod(t *testing.T) {
	w, pool, _ := wakeRuntime(t)
	ctx := context.Background()
	at := time.Now().UTC()
	pause := openLegionPause(t, pool, at.Add(-time.Hour))
	holdLegionPod(t, w.idleStore, pause, "LEGION-208", at.Add(-time.Hour))
	endLegionPause(t, pool, pause, at.Add(-time.Minute))
	ready := supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleImplementer), Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, ServingGeneration: 1, State: supervise.StateReady}
	if err := w.idleStore.PutClaim(ctx, ready); err != nil {
		t.Fatalf("store the implementer ready: %v", err)
	}

	w.podResume(ready, supervise.StateReady)

	testwait.Eventually(t, "the pod's hold ended at the implementer's ready", func() bool {
		return podHoldEnd(t, pool, "LEGION-208") == string(store.PausePodHoldReady)
	})
}

// closeAuthorizingTrees is the store with the issue's close authorized or refused, as act says.
type closeAuthorizingTrees struct {
	outboxTrees
	act bool
}

func (c closeAuthorizingTrees) IssueSuspension(context.Context, string, store.IssueClose, func(string) bool) (bool, error) {
	return c.act, nil
}

// Decision 13 of dispatch://INFRA-3759: a close, a park, a linger or a production check's look
// that deletes the pod ends the pause's hold of it, as deleted; a suspension the store does not
// authorize deletes nothing and leaves the hold standing.
func TestAPodHoldEndsWhenTheIssuesSuspensionDeletesThePod(t *testing.T) {
	for _, tc := range []struct {
		name    string
		act     bool
		end     string
		deleted int
	}{
		{name: "the store authorizes the close", act: true, end: string(store.PausePodHoldDeleted), deleted: 1},
		{name: "the store refuses the close", act: false, end: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			st := outboxTreeStore(t, pool, "LEGION-208")
			ctx := context.Background()
			pause := openLegionPause(t, pool, time.Now().Add(-time.Hour))
			holdLegionPod(t, st, pause, "LEGION-208", time.Now().Add(-time.Hour))
			rt := &releaseRuntime{Runtime: fake.NewRuntime()}
			runner := &outbox{supervisor: &supervisor{deps: supervise.Deps{Runtime: rt}}, trees: closeAuthorizingTrees{outboxTrees: st, act: tc.act},
				project: "legion", dispatchProject: "LEGION", log: quietLogger(), now: time.Now}

			if err := runner.suspendIssue(ctx, record.OutboxRow{ID: 7, Issue: "LEGION-208"},
				record.IssueSuspend{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1}); err != nil {
				t.Fatalf("the issue's suspension: %v", err)
			}
			if _, deleted := rt.reads(); deleted != tc.deleted {
				t.Fatalf("the suspension deleted the pod %d times, want %d", deleted, tc.deleted)
			}
			if got := podHoldEnd(t, pool, "LEGION-208"); got != tc.end {
				t.Fatalf("the pod's hold ended %q, want %q", got, tc.end)
			}
		})
	}
}

// Decision 12 of dispatch://INFRA-3759: no update round opens over a pod a project's pause holds,
// however its configuration drifted; once the hold ends, the next sweep opens the round.
func TestPodUpdateOpensNoRoundOverAPodAPauseHolds(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	pause := openLegionPause(t, rig.pool, podUpdateStart)
	holdLegionPod(t, rig.st, pause, podUpdateIssue, podUpdateStart)

	rig.tick()
	rig.tick()
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if round := rig.claimOf(role).UpdateRound; round != "" {
			t.Fatalf("the %s records round %q over a pod the pause holds, want none", role, round)
		}
	}
	if lines := loggedLines(t, rig.logs.String(), "supervise: a project's pause holds this drifted issue's pod; no update round opens over it until the hold ends"); len(lines) == 0 {
		t.Fatal("the sweep did not say why it opened no round")
	}

	endLegionPause(t, rig.pool, pause, podUpdateStart.Add(time.Minute))
	if _, err := rig.st.EndPausePodHold(rig.ctx, "LEGION", podUpdateIssue, store.PausePodHoldReady, podUpdateStart.Add(time.Minute)); err != nil {
		t.Fatalf("end the pod's hold: %v", err)
	}
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
}

// Decision 12 of dispatch://INFRA-3759: a round over a pod a project's pause holds waits as an
// operator's suspension does, past its bound and telling nobody, and its bound moves by the time
// the pod was held. A hold of ten minutes from five minutes into the round keeps the pod at the
// bound, a minute short of the moved bound the roles still have, and the moved bound replaces it.
func TestPodUpdateRoundWaitsOverAPodAPauseHoldsAndItsBoundMovesByTheHeldTime(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)

	heldAt := podUpdateStart.Add(5 * time.Minute)
	pause := openLegionPause(t, rig.pool, heldAt)
	holdLegionPod(t, rig.st, pause, podUpdateIssue, heldAt)
	rig.clockAt(podUpdateBound)
	rig.tick()
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("at the bound the pod a pause holds was replaced %d times, want none", len(calls))
	}
	if lines := loggedLines(t, rig.logs.String(), "supervise: a project's pause holds the pod of this update round; the replacement waits for the hold to end"); len(lines) == 0 {
		t.Fatal("the sweep did not say the round waits for the pause's hold")
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round a pause holds", notices)
	}

	endedAt := heldAt.Add(10 * time.Minute)
	endLegionPause(t, rig.pool, pause, endedAt)
	if _, err := rig.st.EndPausePodHold(rig.ctx, "LEGION", podUpdateIssue, store.PausePodHoldReady, endedAt); err != nil {
		t.Fatalf("end the pod's hold: %v", err)
	}
	rig.clockAt(podUpdateBound.Add(9 * time.Minute))
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("a minute before the bound the held ten minutes moved, the pod was replaced %d times, want none", len(calls))
	}

	rig.clockAt(podUpdateBound.Add(10 * time.Minute))
	rig.awaitReplacement(1, "at the bound the held ten minutes moved")
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round the moved bound ended", notices)
	}
}

// dispatch://INFRA-4109 R1 and R2 with the project pause carried (task t11 of its plan): a role a
// project's pause stopped is the pause's, not R1's, while the pause keeps its pod. The pause
// stopped the tester, suspended released, held the pod, and held the tester's launch. Its resume
// ended the hold a minute before the round opened, because the implementer was live
// (recordPause). The tester's held start then ran, and the wake pacing parked its launch until ten
// minutes past the round's own bound (parkPauseHeldLaunch): the tester is launching, and it
// records no process.
//
// The round asks the implementer, which answers, and the round waits for the tester while its
// launch is parked. The pod is not replaced at the opening, or a minute past the bound the grace
// alone sets: the hold moved that bound on to the let-out (heldUntil, roundClock.roundBound).
// Nothing asks the tester while its launch is parked, so the round writes no update-ask row for
// it: a publish to a role with no holder wakes it. At the let-out the wake pacing runs the launch.
// The tester comes up in the round's pod, which the implementer's process keeps live
// (roundPodEndsAtLaunch). The round asks the tester once it is ready, and the tester answers. The
// pod is then replaced once, before the moved bound, and the replacement's line names no claim
// the round did not wait for.
//
// A bound that the hold does not move replaces the pod a minute past the bound the grace alone
// sets, while the tester's launch is still parked, and the test fails there.
func TestPodUpdateRoundWaitsForARoleThePauseStoppedUntilTheLetOutOfItsHeldLaunch(t *testing.T) {
	const stopped = claim.RoleTester
	letOut := podUpdateBound.Add(10 * time.Minute)
	rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: pacedUntil(letOut)},
		readyWorker(claim.RoleImplementer), suspendedRole(stopped, supervise.SuspendedByReleased))
	rig.drifts(podUpdateDrift)
	rig.recordPause(store.PausePodHoldResumed, stopped)
	rig.parkPauseHeldLaunch(stopped, letOut)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(stopped, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.settleAll()
	for _, at := range []time.Time{podUpdateStart, podUpdateBound.Add(time.Minute)} {
		rig.clockAt(at)
		rig.tick()
		rig.tick()
		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("at %s, before the let-out %s, the pod was replaced %d times while the %s the pause stopped had not answered",
				at, letOut, len(calls), stopped)
		}
		if notices := rig.notices(); len(notices) != 0 {
			t.Fatalf("at %s, before the moved bound, the controller was told %+v", at, notices)
		}
		if c := rig.claimOf(stopped); c.State != supervise.StateLaunching || c.Locator != nil {
			t.Fatalf("at %s the %s is %s recording %+v, want its launch still parked with no process", at, stopped, c.State, c.Locator)
		}
	}
	if n := rig.askedOf(stopped); n != 0 {
		t.Fatalf("the round wrote %d update-ask rows for the %s while its launch was parked, want none: a publish to a role with no holder wakes it", n, stopped)
	}

	rig.intoRoundPod(stopped)
	rig.runParkedLaunch(stopped, podUpdateStart, letOut)
	rig.launchedIntoRoundPod(stopped)
	rig.registerInto(stopped, podUpdatePod)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times before the %s answered", len(calls), stopped)
	}
	rig.awaitAsked(stopped, podUpdateRound)
	rig.signal(stopped, "e4f5a6b")
	rig.awaitReplacement(1, fmt.Sprintf("once the %s answered", stopped))
	lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	if len(lines) != 1 {
		t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
	}
	if lines[0]["atTheBound"] != false {
		t.Errorf("the replacement's line says atTheBound=%v, want false: the %s answered before the moved bound", lines[0]["atTheBound"], stopped)
	}
	wantField(t, lines[0], "notWaitedFor")
	if n := rig.askedOf(stopped); n != 1 {
		t.Fatalf("the round wrote %d update-ask rows for the %s, want the one once it was ready", n, stopped)
	}
}

// launchKeptLine is what the launch hook logs where it keeps the pod of a round for a launch
// (keepAtLaunch), with the reason it keeps it.
const launchKeptLine = "supervise: a launch goes into the pod of an update round that keeps its pod"

// launchKeptBecause is each reason the launch hook logged for keeping the round's pod for role's
// launch (launchKeptLine), oldest first.
func (r *podUpdateRig) launchKeptBecause(role claim.Role) []string {
	r.t.Helper()
	var reasons []string
	for _, line := range loggedLines(r.t, r.logs.String(), launchKeptLine) {
		if line["claim"] == string(r.tokens[role]) {
			reasons = append(reasons, fmt.Sprint(line["reason"]))
		}
	}
	return reasons
}

// dispatch://INFRA-4109 R1 with the project pause carried, as its architect set the rule in
// review round 3: the round keeps the pod while the pause still owes a launch to any role it
// stopped on the pod, read for each role from the store (store.PausePodHold.Owed, podHolds.owes),
// never from the latest let-out of all the roles. The pause stopped the reviewer and the
// implementer, suspended released, and held the launch of each. Its resume ended the hold a minute
// before the round opened (recordPause). The tester answered the round, and the daemon then
// suspended it. The wake pacing lets each launch out at the moment it reserves it.
//
// The reviewer's held start runs first, and the pacing lets its launch out at once, after the
// hold's end. The reviewer comes up in the round's pod, the round asks it, and it answers. The
// daemon then suspends it, so no process runs in the pod. The implementer's held start has not run
// yet: it is still suspended released, and the pause still owes it a launch. So the round does not
// pass over the implementer, and it waits for it. The pod is not replaced through the reviewer's
// process once the reviewer has answered, and it is not replaced by issue once no process runs in
// it. Then, in the first case, the tester's launch keeps the pod for the implementer's work, and
// the hook's reason names the implementer. In the second case, the implementer's held launch runs
// five minutes into the round, as a start row that failed and backed off runs. It comes up in the
// round's pod with one launch, and the round replaces the pod once the implementer has answered.
//
// Where the keep is read from the latest let-out of the pause, the reviewer's let-out ends the
// keep of both roles. The round then passes over the implementer, and it replaces the pod through
// the reviewer's process once the reviewer has answered, before the implementer's launch.
func TestPodUpdateRoundKeepsThePodForEachRoleThePauseStillOwesALaunch(t *testing.T) {
	const first, second = claim.RoleReviewer, pauseStoppedRole
	// letsOutAtOnce is a wake pacing that lets every paced launch out at the moment it reserves it.
	letsOutAtOnce := supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time { return now }}
	waitsFor := fmt.Sprintf("the held launch of the %s", second)
	// firstRanSecondWaits is a rig of its own, taken to the moment where first's held launch has
	// run and first has answered and has been suspended, and second's held start has not run.
	firstRanSecondWaits := func(t *testing.T) *podUpdateRig {
		t.Helper()
		rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: letsOutAtOnce},
			releasedUnasked(first), releasedUnasked(second), answeredThenSuspended(claim.RoleTester))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.stampsResumes()
		rig.recordPause(store.PausePodHoldResumed, first, second)

		// The daemon writes the let-out on a goroutine of its own when it reserves the launch
		// (projectPause.wakeLetOut), so the write can land on either side of the launch.
		rig.intoRoundPod(first)
		rig.heldStart(first)
		rig.writeLetOut(first, podUpdateStart)
		rig.launchedIntoRoundPod(first)
		rig.registerInto(first, podUpdatePod)
		rig.awaitResumeReady(first)
		rig.awaitAsked(first, podUpdateRound)
		rig.signal(first, "a1b2c3d")
		rig.wantPodKept(waitsFor)

		rig.handle(first, supervise.RequestSuspend{Claim: rig.tokens[first], Reason: "its phase ended", By: supervise.SuspendedByDaemon})
		rig.awaitClaim(first, "the daemon's suspension to let its process go", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil
		})
		rig.wantPodKept(waitsFor)
		if c := rig.claimOf(second); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByReleased ||
			c.UpdateRound != podUpdateRound {
			t.Fatalf("the %s is %s by %q in round %q, want it suspended released in the open round", second, c.State, c.SuspendedBy, c.UpdateRound)
		}
		return rig
	}

	t.Run("a sibling's launch keeps the pod", func(t *testing.T) {
		const sibling = claim.RoleTester
		rig := firstRanSecondWaits(t)
		rig.intoRoundPod(sibling)
		rig.handle(sibling, supervise.RequestResume{Claim: rig.tokens[sibling]})
		rig.launchedIntoRoundPod(sibling)
		if reasons := rig.launchKeptBecause(sibling); len(reasons) != 1 || !strings.Contains(reasons[0], fmt.Sprintf("owes the %s a launch", second)) {
			t.Fatalf("the launch of the %s kept the pod because %q, want once because the pause owes the %s a launch", sibling, reasons, second)
		}
		rig.registerInto(sibling, podUpdatePod)
		rig.wantPodKept(waitsFor)
	})

	t.Run("the held launch comes up in the round's pod", func(t *testing.T) {
		rig := firstRanSecondWaits(t)
		rig.clockAt(podUpdateStart.Add(5 * time.Minute))
		rig.intoRoundPod(second)
		rig.heldStart(second)
		rig.writeLetOut(second, rig.clock.Now())
		rig.launchedIntoRoundPod(second)
		rig.registerInto(second, podUpdatePod)
		rig.awaitAsked(second, podUpdateRound)
		rig.signal(second, "e4f5a6b")
		rig.awaitReplacement(1, fmt.Sprintf("once the %s answered", second))
		lines := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
		if len(lines) != 1 {
			t.Fatalf("the replacement was logged %d times, want once: %+v", len(lines), lines)
		}
		if lines[0]["atTheBound"] != false {
			t.Errorf("the replacement's line says atTheBound=%v, want false: the %s answered before the bound", lines[0]["atTheBound"], second)
		}
		wantField(t, lines[0], "notWaitedFor")
		if asked := rig.byIssueAsks(); len(asked) != 0 {
			t.Errorf("the runtime was asked to replace the pod by issue %+v, want the replacement through the %s's process", asked, second)
		}
	})
}

// dispatch://INFRA-4109 R1 and R7 with the project pause carried, as its architect set the rule
// in review round 3: an open resume of the pause whose role has come up keeps nothing. The pause
// stopped the implementer, suspended released, and held its launch. Its resume ended the hold a
// minute before the round opened (recordPause). The wake pacing is off. The reviewer was released
// by an idle release before the pause, and the round never asked it (releasedUnasked). The tester
// answered the round, and the daemon then suspended it.
//
// The implementer's held start runs. Its launch comes up in the round's pod, which the pause kept
// for it (podHolds.pauseHeldLaunch), and its ready is stamped on its resume as the daemon stamps it
// (stampsResumes). The round asks it. The daemon then suspends it before its first turn, as a
// root's exit does, so no process runs in the pod, and its resume stays open. The implementer has
// run in the pod since the pause stopped it, so the pause owes it nothing more
// (store.PausePodHold.Owed), and the hold, which ended before the round opened, is not read at
// all. So the round passes over the implementer, whose ask holds nothing, and the reviewer (R1).
// In the first case, the sweep replaces the pod by issue before the bound. In the second case, a
// launch of the reviewer comes first. The pause does not keep the pod for it: the launch replaces
// the pod as the round's own replacement and starts the reviewer in the pod in its place, so the
// reviewer is launched once (R7).
//
// Where the open resume is read as owed whatever its ready, the pause keeps the pod until the
// round's bound for each member that records no process. The sweep then replaces nothing, and the
// reviewer's launch goes into the round's pod, which the round replaces later under the reviewer's
// first turn.
func TestAPodlessRoundIsNotKeptForAPauseResumeWhoseRoleCameUp(t *testing.T) {
	const stopped, idle = pauseStoppedRole, claim.RoleReviewer
	members := []claim.Role{stopped, idle, claim.RoleTester}
	// cameUpThenSuspended is a rig of its own, taken to the moment where the daemon has suspended
	// stopped after it came up, with its resume open.
	cameUpThenSuspended := func(t *testing.T) *podUpdateRig {
		t.Helper()
		rig := newPodUpdateRig(t, releasedUnasked(stopped), releasedUnasked(idle), answeredThenSuspended(claim.RoleTester))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.stampsResumes()
		rig.recordPause(store.PausePodHoldResumed, stopped)

		rig.intoRoundPod(stopped)
		rig.heldStart(stopped)
		rig.launchedIntoRoundPod(stopped)
		rig.registerInto(stopped, podUpdatePod)
		rig.awaitResumeReady(stopped)
		rig.awaitAsked(stopped, podUpdateRound)
		rig.handle(stopped, supervise.RequestSuspend{Claim: rig.tokens[stopped], Reason: "the root of its tree exited", By: supervise.SuspendedByDaemon})
		rig.awaitClaim(stopped, "the daemon's suspension to let its process go", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil
		})
		if _, _, open, err := rig.st.OpenPodResume(rig.ctx, podUpdateProject, podUpdateIssue, stopped); err != nil || !open {
			t.Fatalf("the %s's resume is open = %t, %v; want it open: no first turn stamped it and nothing closed it", stopped, open, err)
		}
		return rig
	}

	t.Run("the sweep replaces the pod by issue", func(t *testing.T) {
		rig := cameUpThenSuspended(t)
		replaced := rig.awaitIssueReplacement("once nothing holds it")
		if replaced.Issue != podUpdateIssue || replaced.PodUID != podUpdatePod {
			t.Errorf("the pod was replaced by issue as %+v, want issue %s and the round's pod %s", replaced, podUpdateIssue, podUpdatePod)
		}
		for _, role := range members {
			rig.awaitClaim(role, "its round to end with the replacement by issue", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		}
		wantPodlessEnd(t, rig, false, []claim.Role{stopped, idle}, claim.RoleTester)
	})

	t.Run("a launch into the pod replaces it", func(t *testing.T) {
		rig := cameUpThenSuspended(t)
		rig.intoRoundPod(idle)
		rig.handle(idle, rig.lookStart(idle))
		c := rig.awaitClaim(idle, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
		pod := podOf(c.Locator)
		if pod == podUpdatePod {
			t.Fatalf("the %s was launched into the round's pod %s, which the round replaces later under its first turn (R7), want the pod in its place; the hook kept the pod because %q",
				idle, pod, rig.launchKeptBecause(idle))
		}
		if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 1 || replaced[0].PodUID != podUpdatePod {
			t.Fatalf("the launch replaced %+v, want the round's pod %s once", replaced, podUpdatePod)
		}
		rig.registerInto(idle, pod)
		for _, role := range members {
			rig.awaitSwept(role, "its round to end as the launch's replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		}
		wantRoundPodEnd(t, rig, "atLaunch", false, []claim.Role{stopped, idle}, claim.RoleTester)
		if launches := rig.launchesOf(idle); len(launches) != 1 {
			t.Errorf("the %s was launched %d times, want once", idle, len(launches))
		}
		if asked := rig.byIssueAsks(); len(asked) != 0 {
			t.Errorf("the runtime was asked to replace the pod by issue %+v, want the launch's replacement alone", asked)
		}
	})
}
