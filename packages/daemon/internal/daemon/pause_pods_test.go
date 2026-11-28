package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
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
	t.Parallel()
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
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
	t.Parallel()
	w, pool, logged := wakeRuntime(t)
	ctx := context.Background()
	woke := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	letOut := woke.Add(30 * time.Minute)
	for _, issue := range []string{"LEGION-208", "OTHERPROJECT-5"} {
		token := claim.Token("legion-legion-" + strings.ToLower(issue) + "-implementer")
		recordPodWake(t, w.idleStore, store.PodResume{Project: "legion", Issue: issue,
			Role: string(claim.RoleImplementer), Claim: string(token), Kind: pods.WakeBackstop, At: woke})
		if _, err := w.idleStore.SetResumeClock(ctx, "legion", issue, token, letOut); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// project's pause stopped is the pause's, not R1's, while the pause still owes that role a launch
// (podHolds.owes). The pause stopped the tester, suspended released, held the pod, and held the
// tester's launch. Its resume ended the hold a minute before the round opened, because the
// implementer was live (recordPause). The tester's held start then ran, and the wake pacing parked
// its launch until ten minutes past the round's own bound (parkPauseHeldLaunch): the tester is
// launching, and it records no process. The daemon writes that let-out (writeLetOut).
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
	t.Parallel()
	const stopped = claim.RoleTester
	letOut := podUpdateBound.Add(10 * time.Minute)
	rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: pacedUntil(letOut)},
		readyWorker(claim.RoleImplementer), suspendedRole(stopped, supervise.SuspendedByReleased))
	rig.drifts(podUpdateDrift)
	rig.recordPause(store.PausePodHoldResumed, stopped)
	rig.parkPauseHeldLaunch(stopped, letOut)
	rig.writeLetOut(stopped, letOut)

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

// wantLaunchKeptForOwed holds the launch hook to one reason for keeping the round's pod for role's
// launch (launchKeptBecause): a project's pause owes the owed role a launch (roundPodEnds).
func (r *podUpdateRig) wantLaunchKeptForOwed(role, owed claim.Role) {
	r.t.Helper()
	if reasons := r.launchKeptBecause(role); len(reasons) != 1 || !strings.Contains(reasons[0], fmt.Sprintf("owes the %s a launch", owed)) {
		r.t.Fatalf("the launch of the %s kept the pod because %q, want once because the pause owes the %s a launch", role, reasons, owed)
	}
}

// dispatch://INFRA-4109 R1 with the project pause carried, as its architect set the rule in
// review round 3: the round keeps the pod while the pause still owes a launch to any claim it
// stopped on the pod, read for each claim from the store (store.PausePodHold.Owed, podHolds.owes),
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
	t.Parallel()
	const first, second = claim.RoleReviewer, pauseStoppedRole
	waitsFor := fmt.Sprintf("the held launch of the %s", second)
	// firstRanSecondWaits is a rig of its own, taken to the moment where first's held launch has
	// run and first has answered and has been suspended, and second's held start has not run.
	firstRanSecondWaits := func(t *testing.T) *podUpdateRig {
		t.Helper()
		rig := newPodUpdateRigWith(t, podUpdateRigOptions{wakePace: lettingOutAtOnce},
			releasedUnasked(first), releasedUnasked(second), answeredThenSuspended(claim.RoleTester))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.stampsResumes()
		rig.recordPause(store.PausePodHoldResumed, first, second)

		// The daemon writes the let-out on a goroutine of its own when the pacer lets the launch
		// out (projectPause.wakeLetOut), so the write can land on either side of the launch.
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
		rig.handle(sibling, supervise.RequestResume{Claim: rig.tokens[sibling], By: supervise.SuspendedByOperator})
		rig.launchedIntoRoundPod(sibling)
		rig.wantLaunchKeptForOwed(sibling, second)
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

// dispatch://INFRA-4109 review round 5: a daemon restart while the wake pacing parks the launch
// that a project's pause held does not let the sweep replace the pod that the pause kept. The
// members are those of pauseKeepsThePod: the implementer, which the pause stopped and the round
// never asked, and the tester, which answered and which the daemon then suspended. The
// implementer's held start runs, and the pacing parks its launch (parkPauseHeldLaunch), so the
// store holds the claim launching with no process. The daemon stops, and the next daemon boots
// over the same store. Its restore puts the claim back to launch_uncertain, as it does every
// launch with no process recorded (supervisor.restore). Its orphan reconciliation fails, so it does
// not launch the claim again (reconcileBootOrphans). The sweeps before the bound keep the pod. The
// next reconciliation succeeds, and the daemon releases the launch to queued, the first of the two
// steps of its relaunch (releaseUnfinishedClaim, launchUnfinished). The sweeps still keep the pod.
// The second step, the spawn, waits in the wake pacing until the let-out. At the let-out the launch
// comes up in the round's pod with one launch, and the hook's reason names the launch that the
// pause owes.
//
// Where the keep is read of a launching claim alone (review round 4), the claim restored
// launch_uncertain is no launch the pause owes, and the first sweep after the restart replaces the
// pod by issue.
func TestAPodlessRoundKeepsThePodForAHeldLaunchThatARestartLeftUncertain(t *testing.T) {
	t.Parallel()
	const stopped = pauseStoppedRole
	pauseKeepsThePod(t, func(t *testing.T, rig *podUpdateRig) {
		rig.parkPauseHeldLaunch(stopped, pauseLetOut)
		restartedAt := podUpdateStart.Add(time.Minute)
		rig.clockAt(restartedAt)
		next := rig.restart()
		next.s.log = rig.log
		next.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		next.rt.FailReconcileOrphans(errors.New("the Sandbox list timed out"))
		if next.s.reconcileBootOrphans(next.ctx) {
			t.Fatal("the boot's orphan reconciliation passed, want it failed as the runtime failed it")
		}
		if c := next.claimOf(stopped); c.State != supervise.StateLaunchUncertain || c.Locator != nil {
			t.Fatalf("the %s after the restart is %s recording %+v, want launch_uncertain with no process", stopped, c.State, c.Locator)
		}
		next.wantPodKept(fmt.Sprintf("the launch of the %s that the restart left uncertain", stopped))

		next.rt.FailReconcileOrphans(nil)
		if !next.s.reconcileBootOrphans(next.ctx) {
			t.Fatal("the next orphan reconciliation failed, want it passed")
		}
		m, _ := next.sup.Machine(next.tokens[stopped])
		if released, err := m.ReleaseUncertainLaunch(next.ctx); err != nil || !released {
			t.Fatalf("release the %s's uncertain launch = %t, %v; want it released to queued", stopped, released, err)
		}
		if c := next.claimOf(stopped); c.State != supervise.StateQueued || c.Locator != nil {
			t.Fatalf("the %s after its release is %s recording %+v, want queued with no process", stopped, c.State, c.Locator)
		}
		next.wantPodKept(fmt.Sprintf("the launch of the %s that the restart released", stopped))

		next.intoRoundPod(stopped)
		next.handle(stopped, supervise.RequestSpawn{Claim: next.tokens[stopped]})
		next.wantLaunchParked(stopped, pauseLetOut)
		next.runParkedLaunch(stopped, restartedAt, pauseLetOut)
		next.launchedIntoRoundPod(stopped)
		next.wantLaunchKeptForOwed(stopped, stopped)
	})
}

// dispatch://INFRA-4109 review round 5: the launch that a project's pause still owes keeps the pod
// that the pause kept where the launch forgets the role's session. The members are those of
// pauseKeepsThePod, with the wake pacing off: the implementer, which the pause stopped and the
// round never asked, and the tester, which answered and which the daemon then suspended. The
// implementer's held start runs. No session store holds its recorded session any more, so the
// launch forgets the session and spawns a fresh agent in the same try, charged nothing
// (supervise's startFresh). The runtime refuses that spawn, which is a charged launch failure,
// and the next try of the same launch spawns again into the round's pod. That try writes the
// claim with no session, so the round's sets do not read it (roundAsks). The launch hook reads it
// as the launch that the pause owes all the same (roundPodEnds over the whole group,
// podHolds.owes): the launch keeps the pod and comes up in it, and the hook's reason names the
// implementer.
//
// Where the keep is read only of the claims the round waits for (review round 4), the hook reads
// no launch of the pause, and the launch replaces the pod that the pause kept as the round's own
// replacement.
func TestALaunchUnderARoundKeepsThePodWhereTheLaunchAPauseOwesForgetsItsSession(t *testing.T) {
	t.Parallel()
	const stopped = pauseStoppedRole
	rig := newPodUpdateRig(t, releasedUnasked(stopped), answeredThenSuspended(claim.RoleTester))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	rig.recordPause(store.PausePodHoldResumed, stopped)
	token := rig.tokens[stopped]
	rig.scriptForgottenSession(stopped, nil)
	rig.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	conn := fake.NewConn()
	rig.conns.Register(token, conn)
	rig.workers[stopped] = conn

	rig.heldStart(stopped)
	c := rig.awaitClaim(stopped, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
	if pod := podOf(c.Locator); pod != podUpdatePod {
		t.Fatalf("the %s was launched into pod %q, want the round's own pod %s, which the pause kept; the hook kept the pod because %q",
			stopped, pod, podUpdatePod, rig.launchKeptBecause(stopped))
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		t.Fatalf("a launch replaced the round's pod %+v, which the pause kept", replaced)
	}
	if c.Session != "" || c.Budgets.LaunchFailures != 1 {
		t.Fatalf("the %s records session %q after %d launch failures, want its session forgotten and the one refused spawn charged",
			stopped, c.Session, c.Budgets.LaunchFailures)
	}
	var methods []string
	for _, launch := range rig.launchesOf(stopped) {
		methods = append(methods, launch.Method)
	}
	if strings.Join(methods, " ") != "Resume Spawn Spawn" {
		t.Fatalf("the %s was launched by %v, want the resume of its lost session, the refused fresh spawn and the spawn that came up", stopped, methods)
	}
	rig.wantLaunchKeptForOwed(stopped, stopped)
}

// byIssueNotDueLine is the line of a replacement by issue that the daemon's last word refused
// under the pod's launch turn (issueReplacementDue). Its reason field is that refusal, which names
// what keeps the pod.
const byIssueNotDueLine = "supervise: this update round's replacement by issue was no longer due when its pod's launch turn came; nothing was replaced and the round stays open"

// underLaunchTurn has the next replacement that hold stops under the pod's launch turn (fake's
// HoldReplacePod or HoldReplaceIssuePod) run act there. The hook clears itself before act runs,
// so the sweeps after that replacement find the runtime as it left it. It sweeps until that
// replacement has run act, and so has asked its last word. what names that event in the wait's
// failure.
func (r *podUpdateRig) underLaunchTurn(hold func(func()), what string, act func()) {
	r.t.Helper()
	ran := false
	hold(func() {
		hold(nil)
		act()
		ran = true
	})
	testwait.Eventually(r.t, what, func() bool {
		r.tick()
		return ran
	})
}

// takeHeldWakeUnderLaunchTurn has the next replacement that hold stops under the pod's launch turn
// take a wake of role there, held by pause (underLaunchTurn), as the outbox takes the delivery of
// a row that pause held (store.Store.TakePodWake, outbox's wakeForDelivery). The wake opens a
// resume of pause for role (store.PodRecords.PauseOfWake).
func (r *podUpdateRig) takeHeldWakeUnderLaunchTurn(hold func(func()), role claim.Role, pause int64) {
	r.t.Helper()
	r.underLaunchTurn(hold, fmt.Sprintf("a replacement to take the wake of the %s under the pod's launch turn", role), func() {
		taken, err := r.st.TakePodWake(r.ctx, podUpdateProject, podUpdateIssue, r.tokens[role], role, pods.WakeDelivery, r.clock.Now(), pause)
		if err != nil || !taken {
			r.t.Fatalf("take the wake of the %s that the pause held = %t, %v; want it taken", role, taken, err)
		}
	})
}

// holdPodUnderLaunchTurn has the next replacement that hold stops under the pod's launch turn
// find a project's pause that holds the issue's pod (underLaunchTurn): a pause of LEGION opens
// there, and it holds the pod at the rig's clock, as recordPause records a pause and its hold
// (openLegionPause, holdLegionPod). The pause's hold of the pod stays after that replacement.
func (r *podUpdateRig) holdPodUnderLaunchTurn(hold func(func())) {
	r.t.Helper()
	r.underLaunchTurn(hold, "a replacement to find the pod held by a pause under the pod's launch turn", func() {
		pause := openLegionPause(r.t, r.pool, r.clock.Now())
		holdLegionPod(r.t, r.st, pause, podUpdateIssue, r.clock.Now())
	})
}

// wantRefusedUnderTurn holds the daemon's log to one refusal of a replacement under the pod's
// launch turn: line is logged once, and the reason field of that line holds reason.
func (r *podUpdateRig) wantRefusedUnderTurn(line, reason string) {
	r.t.Helper()
	refusals := loggedMessages(r.t, r.logs.String(), line)
	if len(refusals) != 1 {
		r.t.Fatalf("the refusal under the pod's launch turn was logged %d times, want once: %+v", len(refusals), refusals)
	}
	if got := fmt.Sprint(refusals[0]["reason"]); !strings.Contains(got, reason) {
		r.t.Errorf("the refusal's reason is %q, want it to hold %q", got, reason)
	}
}

// dispatch://INFRA-4109 review round 5: the last word on a replacement through a process reads
// the project pause's holds again under the pod's launch turn (replacementDue). A project's pause
// held the pod and stopped no role. Its resume ended the hold a minute before the round opened
// (recordPause). So when the sweep reads the holds, the pause owes no role a launch. An idle
// release suspended the reviewer, so the round passes over it (R1). The implementer answers, the
// read of the workspace finds no work, and the sweep goes to the replacement. While the runtime
// waits for the pod's launch turn, the outbox takes the delivery of a row that the pause held for
// the reviewer. That wake opens a resume of the pause for the reviewer
// (store.PodRecords.PauseOfWake). The pause now owes the reviewer a launch, so the round no
// longer passes over the reviewer (podHolds.owes). The last word refuses the replacement because
// the reviewer has not answered, and the next sweeps keep the pod.
//
// Where the last word reads the holds the sweep read, the round still passes over the reviewer,
// and the replacement deletes the pod that the pause keeps for the reviewer's work.
func TestPodUpdateReplacementReadsThePauseHoldsAgainUnderTheLaunchTurn(t *testing.T) {
	t.Parallel()
	const woken = claim.RoleReviewer
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), suspendedRole(woken, supervise.SuspendedByReleased))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead})
	rig.drifts(podUpdateDrift)
	pause := rig.recordPause(store.PausePodHoldResumed)

	rig.tick()
	rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(woken, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.settleAll()
	rig.takeHeldWakeUnderLaunchTurn(rig.rt.HoldReplacePod, woken, pause)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times, want none: the pause owes the %s a launch", len(calls), woken)
	}
	rig.wantRefusedUnderTurn(backgroundNotDueLine, fmt.Sprintf("%s has not answered it, or its machine was deciding something", woken))
	rig.wantPodKept(fmt.Sprintf("the answer of the %s", woken))
}

// dispatch://INFRA-4109 review round 5: the last word on a replacement by issue reads the project
// pause's holds again under the pod's launch turn (issueReplacementDue). The members are those of
// pauseKeepsThePod, with the wake pacing off: the implementer, which an idle release suspended
// before the round's ask reached it, and the tester, which answered and which the daemon then
// suspended. No process runs in the pod. A project's pause held the pod and stopped no role. Its
// resume ended the hold a minute before the round opened (recordPause). So when the sweep reads
// the holds, the pause owes no role a launch, nothing holds the pod, and the sweep goes to the
// replacement by issue. While the runtime waits for the pod's launch turn, the outbox takes the
// delivery of a row that the pause held for the implementer. That wake opens a resume of the pause
// for the implementer (store.PodRecords.PauseOfWake). The pause now owes the implementer a launch,
// so it keeps the pod for the implementer's work (roundPodEnds, podHolds.owes). The last word
// refuses the replacement and names that launch, and the next sweeps ask for no replacement by
// issue.
//
// Where the last word reads the holds the sweep read, nothing holds the pod, and the replacement
// by issue deletes the pod that the pause keeps for the implementer's work.
func TestTheByIssueReplacementReadsThePauseHoldsAgainUnderTheLaunchTurn(t *testing.T) {
	t.Parallel()
	const woken = pauseStoppedRole
	rig := newPodUpdateRig(t, releasedUnasked(woken), answeredThenSuspended(claim.RoleTester))
	rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	pause := rig.recordPause(store.PausePodHoldResumed)

	rig.takeHeldWakeUnderLaunchTurn(rig.rt.HoldReplaceIssuePod, woken, pause)

	if calls := rig.issueReplacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced by issue %d times, want none: the pause owes the %s a launch", len(calls), woken)
	}
	rig.wantRefusedUnderTurn(byIssueNotDueLine, fmt.Sprintf("owes the %s a launch", woken))
	rig.tick()
	rig.tick()
	rig.settleAll()
	if asked := rig.byIssueAsks(); len(asked) != 1 || asked[0].Method != "ReplaceIssuePodNotDue" {
		t.Fatalf("the runtime was asked to replace the pod by issue %+v, want only the call the last word refused", asked)
	}
	if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
		t.Fatalf("a launch replaced the round's pod %+v, which the pause keeps", replaced)
	}
}

// dispatch://INFRA-4109 review round 6: each last word on a replacement reads whether a project's
// pause holds the pod from the holds it reads again under the pod's launch turn (clockUnderTurn,
// roundClock.held). It makes no other read of the hold. A hold that stands on the pod refuses the
// replacement whatever the clock says (decision 12 of dispatch://INFRA-3759). In each case no
// pause holds the pod when the sweep reads the holds, so the sweep goes to the replacement. While
// the runtime waits for the pod's launch turn, a project's pause opens and holds the pod
// (holdPodUnderLaunchTurn). Only the last word can see that hold.
func TestTheLastWordRefusesAReplacementWhileAPauseHoldsThePod(t *testing.T) {
	t.Parallel()
	// The last word on a replacement through a process (replacementDue). The implementer is the
	// round's one member. It answers, and the round is complete. The last word refuses the
	// replacement and names the pause's hold. The next sweeps read the hold too, and they keep the
	// pod.
	//
	// Where the last word does not read the hold, the replacement deletes the pod that the pause
	// holds.
	t.Run("through a process", func(t *testing.T) {
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
		rig.drifts(podUpdateDrift)

		rig.tick()
		rig.awaitAsked(claim.RoleImplementer, podUpdateRound)
		rig.signal(claim.RoleImplementer, "a1b2c3d")
		rig.settleAll()
		rig.holdPodUnderLaunchTurn(rig.rt.HoldReplacePod)

		if calls := rig.replacements(); len(calls) != 0 {
			t.Fatalf("the pod was replaced %d times, want none: a project's pause holds it", len(calls))
		}
		rig.wantRefusedUnderTurn(backgroundNotDueLine, "a project's pause holds the pod")
		rig.wantPodKept("the end of the pause's hold")
	})

	// The last word on a replacement by issue (issueReplacementDue), which reads the hold among
	// what keeps the pod (issuePodKept, roundPodEnds). The members are those of pauseKeepsThePod,
	// and the pause stopped neither of them: the implementer, which an idle release suspended
	// before the round's ask reached it, and the tester, which answered and which the daemon then
	// suspended. No process runs in the pod and nothing holds it, so the sweep goes to the
	// replacement by issue. The last word refuses the replacement and names the pause's hold, and
	// the next sweeps ask for no replacement by issue.
	//
	// Where the last word does not read the hold, the replacement by issue deletes the pod that the
	// pause holds.
	t.Run("by issue", func(t *testing.T) {
		rig := newPodUpdateRig(t, releasedUnasked(claim.RoleImplementer), answeredThenSuspended(claim.RoleTester))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)

		rig.holdPodUnderLaunchTurn(rig.rt.HoldReplaceIssuePod)

		if calls := rig.issueReplacements(); len(calls) != 0 {
			t.Fatalf("the pod was replaced by issue %d times, want none: a project's pause holds it", len(calls))
		}
		rig.wantRefusedUnderTurn(byIssueNotDueLine, "a project's pause holds the pod")
		rig.tick()
		rig.tick()
		rig.settleAll()
		if asked := rig.byIssueAsks(); len(asked) != 1 || asked[0].Method != "ReplaceIssuePodNotDue" {
			t.Fatalf("the runtime was asked to replace the pod by issue %+v, want only the call the last word refused", asked)
		}
		if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 0 {
			t.Fatalf("a launch replaced the round's pod %+v, which the pause holds", replaced)
		}
	})
}

// dispatch://INFRA-4109 R1 and R7 with the project pause carried, as its architect set the rule
// in review round 3: an open resume of the pause whose role has come up keeps nothing. The pause
// stopped the implementer, suspended released, and held its launch. Its resume ended the hold a
// minute before podUpdateStart, because the tester was live (recordPause). The wake pacing is off.
// The reviewer was released by an idle release before the pause.
//
// The implementer's held start runs, and its launch comes up in the issue's pod. Its ready is
// stamped on its resume as the daemon stamps it (stampsResumes). The daemon then suspends it
// before its first turn, as a root's exit does, and its resume stays open. The implementer has run
// in the pod since the pause stopped it, so the pause owes it nothing more
// (store.PausePodHold.Owed), and the hold, which ended before any round opened, is not read at
// all. Five minutes later the pod's configuration drifts, and the sweep opens a round over the pod.
// The round is posted to each claim with a session, and it asks the tester alone: it passes over
// the implementer and the reviewer, which the daemon suspended (R1), so neither holds an ask of
// the round. The tester answers, and the daemon then suspends it, so no process runs in the pod.
//
// Nothing holds the pod. In the first case, the sweep replaces it by issue before the bound. In
// the other two cases, a launch into the pod comes first, before any sweep: the reviewer's, or the
// implementer's own, which its open resume of the pause does not make a launch the pause still
// owes (store.PausePodHold.Owed, podHolds.owes). The pause keeps the pod for neither. Each launch
// replaces the pod as the round's own replacement and starts its role in the pod in its place, so
// the role is launched once (R7).
//
// Where the open resume is read as owed whatever its ready, the implementer's own launch is read
// as the launch the pause held, so it goes into the round's pod, which the round replaces later
// under its first turn. Where the keep is read from the latest let-out of the pause (review round
// 2), the pause keeps the pod for the reviewer too: the sweep replaces nothing before the bound,
// and the reviewer's launch goes into the round's pod.
func TestAPodlessRoundIsNotKeptForAPauseResumeWhoseRoleCameUp(t *testing.T) {
	t.Parallel()
	const stopped, idle = pauseStoppedRole, claim.RoleReviewer
	members := []claim.Role{stopped, idle, claim.RoleTester}
	// cameUpThenARoundOpened is a rig of its own, taken to the moment where the daemon has
	// suspended stopped after it came up, with its resume open, and a round that opened later has
	// no role process in its pod.
	cameUpThenARoundOpened := func(t *testing.T) *podUpdateRig {
		t.Helper()
		rig := newPodUpdateRig(t, suspendedRole(stopped, supervise.SuspendedByReleased),
			suspendedRole(idle, supervise.SuspendedByReleased), readyWorker(claim.RoleTester))
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
		rig.stampsResumes()
		rig.recordPause(store.PausePodHoldResumed, stopped)

		rig.intoPod(stopped, podUpdatePod)
		rig.heldStart(stopped)
		rig.awaitClaim(stopped, "its held launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
		rig.registerInto(stopped, podUpdatePod)
		rig.awaitResumeReady(stopped)
		rig.handle(stopped, supervise.RequestSuspend{Claim: rig.tokens[stopped], Reason: "the root of its tree exited", By: supervise.SuspendedByDaemon})
		rig.awaitClaim(stopped, "the daemon's suspension to let its process go", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil
		})
		if _, _, open, err := rig.st.OpenPodResume(rig.ctx, podUpdateProject, podUpdateIssue, rig.tokens[stopped]); err != nil || !open {
			t.Fatalf("the %s's resume is open = %t, %v; want it open: no first turn stamped it and nothing closed it", stopped, open, err)
		}

		rig.clockAt(podUpdateStart.Add(5 * time.Minute))
		rig.drifts(podUpdateDrift)
		rig.awaitAsked(claim.RoleTester, podUpdateRound)
		for _, role := range []claim.Role{stopped, idle} {
			if c := rig.awaitRound(role, podUpdateRound); c.UpdateTaskedAt != nil {
				t.Fatalf("the %s was asked at %v, want no ask: the round passes over it", role, c.UpdateTaskedAt)
			}
		}
		rig.signal(claim.RoleTester, "a1b2c3d")
		rig.handle(claim.RoleTester, supervise.RequestSuspend{Claim: rig.tokens[claim.RoleTester], Reason: "its phase ended", By: supervise.SuspendedByDaemon})
		rig.awaitClaim(claim.RoleTester, "the daemon's suspension to let its process go", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil
		})
		return rig
	}
	// launchReplacesThePod launches role into the pod by a start row, before any sweep, and holds
	// that launch to replacing the pod as the round's own replacement (R7): role comes up in the pod
	// in its place with one launch, the next sweeps end the round as that replacement, and nothing
	// replaces the pod by issue.
	launchReplacesThePod := func(t *testing.T, rig *podUpdateRig, role claim.Role) {
		t.Helper()
		before := len(rig.launchesOf(role))
		rig.intoRoundPod(role)
		rig.handle(role, rig.lookStart(role))
		c := rig.awaitClaim(role, "its launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil })
		pod := podOf(c.Locator)
		if pod == podUpdatePod {
			t.Fatalf("the %s was launched into the round's pod %s, which the round replaces later under its first turn (R7), want the pod in its place; the hook kept the pod because %q",
				role, pod, rig.launchKeptBecause(role))
		}
		if replaced := rig.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(replaced) != 1 || replaced[0].PodUID != podUpdatePod {
			t.Fatalf("the launch replaced %+v, want the round's pod %s once", replaced, podUpdatePod)
		}
		rig.registerInto(role, pod)
		for _, member := range members {
			rig.awaitSwept(member, "its round to end as the launch's replacement", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		}
		wantRoundPodEnd(t, rig, "atLaunch", false, []claim.Role{stopped, idle}, claim.RoleTester)
		if launches := len(rig.launchesOf(role)) - before; launches != 1 {
			t.Errorf("the %s was launched %d times under the round, want once", role, launches)
		}
		if asked := rig.byIssueAsks(); len(asked) != 0 {
			t.Errorf("the runtime was asked to replace the pod by issue %+v, want the launch's replacement alone", asked)
		}
	}

	t.Run("the sweep replaces the pod by issue", func(t *testing.T) {
		rig := cameUpThenARoundOpened(t)
		replaced := rig.awaitIssueReplacement("once nothing holds it")
		if replaced.Issue != podUpdateIssue || replaced.PodUID != podUpdatePod {
			t.Errorf("the pod was replaced by issue as %+v, want issue %s and the round's pod %s", replaced, podUpdateIssue, podUpdatePod)
		}
		for _, role := range members {
			rig.awaitClaim(role, "its round to end with the replacement by issue", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		}
		wantPodlessEnd(t, rig, false, []claim.Role{stopped, idle}, claim.RoleTester)
	})

	t.Run("a launch of another role replaces the pod", func(t *testing.T) {
		launchReplacesThePod(t, cameUpThenARoundOpened(t), idle)
	})

	t.Run("the launch of the role that came up replaces the pod", func(t *testing.T) {
		launchReplacesThePod(t, cameUpThenARoundOpened(t), stopped)
	})
}
