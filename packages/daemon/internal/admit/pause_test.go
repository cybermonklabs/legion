package admit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// pauseTestProject opens a pause of the package's project at at, as the operator's pause writes
// its row (store's Pause, project_pauses), and answers its id.
func pauseTestProject(t *testing.T, pool *pgxpool.Pool, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into project_pauses (project, began_at, by_kind, reason)
		values ($1, $2, 'operator', 'the model provider is down') returning id`, testProject, at).Scan(&id); err != nil {
		t.Fatalf("pause %s: %v", testProject, err)
	}
	return id
}

// endTestPause ends pause id at at, as the operator's resume ends it (store's Resume).
func endTestPause(t *testing.T, pool *pgxpool.Pool, id int64, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update project_pauses
		set ended_at = $2, ended_by_kind = 'operator', ended_by_session = '' where id = $1`, id, at); err != nil {
		t.Fatalf("resume %s: %v", testProject, err)
	}
}

// slottedKeys is the issues holding one of the cap's slots, in slot order.
func slottedKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	for _, slot := range slots(t, pool) {
		keys = append(keys, slot.Issue)
	}
	return keys
}

// A paused project's admission passes promote nothing and send no role the work of its entry
// (decision 11 of dispatch://INFRA-3759): the tick's pass, the entry sweep and a live fact's pass
// each return at their top while an open pause names the project, so a root waiting in todo with
// a slot free keeps waiting, and a worker behind the entry its phases row names is sent nothing.
// The tick still wakes the controller. Once the pause ends, the same tick promotes the root and
// sends the entry's work: that is what tells the pause's gate from a fixture that would have
// promoted nothing anyway.
func TestAPausedProjectPromotesNothingUntilItsPauseEnds(t *testing.T) {
	pool := migratedPool(t)
	behind, taken := seedConvergeRows(t, pool, convergeRows()[:1])
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	registerController(t, pool)
	admission := quiet(t, taken+1)
	held := slottedKeys(t, pool)

	pause := pauseTestProject(t, pool, fixedNow)
	tick(t, pool, admission, "paused-tick")
	applySweep(t, pool, admission, "paused-sweep")
	apply(t, pool, admission, "paused-fact", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: "LEGION-NEXT", Rank: "B", HandedOver: handed}, engineStub{})

	if got := slottedKeys(t, pool); !slices.Equal(got, held) {
		t.Fatalf("slots while paused = %v, want %v alone: a paused project promotes nothing", got, held)
	}
	if got := issue(t, pool, "LEGION-NEXT"); got.Status != "todo" {
		t.Fatalf("LEGION-NEXT stands %q while paused, want todo: it was promoted", got.Status)
	}
	for _, c := range behind {
		if starts := convergeStarts(t, pool, c.key); len(starts) != 0 {
			t.Fatalf("start rows of %s while paused = %+v, want none: a paused project sends no owed entry", c.key, starts)
		}
	}
	tickNoticeOf(t, pool, testProject)

	endTestPause(t, pool, pause, fixedNow)
	tick(t, pool, admission, "resumed-tick")
	if got := slottedKeys(t, pool); !slices.Equal(got, append(held, "LEGION-NEXT")) {
		t.Fatalf("slots once the pause ended = %v, want %v and LEGION-NEXT", got, held)
	}
	assertConverged(t, pool, behind)
}

// The resume's one admission pass is a fact of its own (intake.ProjectResumed, decision 22 of
// dispatch://INFRA-3759): admission promotes on it as on every fact that is not a Dispatch issue,
// so the root the pause kept waiting takes the free slot at once. It is no controller tick: the
// controller is registered, and no tick notice is queued.
func TestTheResumesPassPromotesAndQueuesNoTickNotice(t *testing.T) {
	pool := migratedPool(t)
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	registerController(t, pool)
	admission := quiet(t, 1)
	pause := pauseTestProject(t, pool, fixedNow)
	apply(t, pool, admission, "paused-fact", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: "LEGION-NEXT", Rank: "B", HandedOver: handed}, engineStub{})
	if got := slottedKeys(t, pool); len(got) != 0 {
		t.Fatalf("slots while paused = %v, want none", got)
	}

	endTestPause(t, pool, pause, fixedNow)
	if _, err := intake.ApplyFact(context.Background(), pool, "pause", "project-resumed", intake.ProjectResumed{},
		engineStub{}, admission); err != nil {
		t.Fatalf("apply the resume's pass: %v", err)
	}

	if got := slottedKeys(t, pool); !slices.Equal(got, []string{"LEGION-NEXT"}) {
		t.Fatalf("slots after the resume's pass = %v, want LEGION-NEXT: the pass promotes", got)
	}
	var ticks int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox
		where kind = 'controller_notice' and payload->>'kind' = 'tick'`).Scan(&ticks); err != nil {
		t.Fatalf("read the tick notices: %v", err)
	}
	if ticks != 0 {
		t.Fatalf("tick notices after the resume's pass = %d, want none: the resume wakes nobody by itself", ticks)
	}
}

// The architect-stalled arms count no time a pause held the project (decision 14 and departure 2
// of dispatch://INFRA-3759, which restart the stuck clocks at the resume): while the pause stood,
// the project's architects were stopped and every start and task the daemon would send them was
// held, so each arm whose bound is a stretch of time counts it from no earlier than the end of the
// project's latest pause. Four roots stand from a minute before a pause of two hours, and each
// would be stalled by one arm if the pause's time counted:
//
//   - slow stands at admitted with no step of its design gate registered (the gate arm, 90
//     minutes);
//   - unsent has an architect the pause stopped, which holds the unsent task of its row's entry
//     (the took-nothing arm, ten minutes);
//   - unclaimed has no architect claim at all, and the work of its entry was raised (the
//     unreachable arm's clause for a root with no claim, ten minutes);
//   - failed has a failed architect claim whose start was queued before the pause, which the
//     pause then held (the unreachable arm's queued start, ten minutes).
//
// While the project is paused, the state's reading names no arm. The resume's own pass tells the
// controller nothing and writes no mark. The three ten-minute arms tell the controller once their
// bound has passed after the pause's end, and the gate arm once its own bound has passed. Each
// reason that names a moment says that the time was counted from the pause's end. So the quiet
// comes from the pause's end, and not from a fixture that no arm would read.
func TestTheArchitectStalledArmsCountNoTimeAPauseHeldTheProject(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	registerController(t, pool)
	admission := quiet(t, 4)
	now := fixedNow
	admission.now = func() time.Time { return now }
	began, ended := fixedNow.Add(-2*time.Hour), fixedNow
	before := began.Add(-time.Minute)
	const slow, unsent, unclaimed, failed = "LEGION-860", "LEGION-861", "LEGION-862", "LEGION-863"
	roots := []string{slow, unsent, unclaimed, failed}

	fixture := architectWakeRoot(slow)
	fixture.admittedAt, fixture.raisedAt, fixture.queuedAt = before, before, before
	seedArchitectWakeRoot(t, pool, fixture)

	fixture = architectWakeRoot(unsent)
	fixture.registered, fixture.state, fixture.confirmed = true, supervise.StateSuspended, false
	fixture.admittedAt, fixture.raisedAt, fixture.queuedAt = before, before, before
	seedArchitectWakeRoot(t, pool, fixture)
	if _, err := pool.Exec(ctx, `update claims set suspended_by = $2 where token = $1`,
		string(architectToken(t, unsent)), string(supervise.SuspendedByReleased)); err != nil {
		t.Fatalf("record the architect of %s as stopped by the pause: %v", unsent, err)
	}

	fixture = architectWakeRoot(unclaimed)
	fixture.registered, fixture.claimed, fixture.task = true, false, false
	fixture.admittedAt, fixture.raisedAt = before, before
	seedArchitectWakeRoot(t, pool, fixture)

	fixture = architectWakeRoot(failed)
	fixture.registered, fixture.state, fixture.task = true, supervise.StateFailed, false
	fixture.admittedAt, fixture.raisedAt = before, before
	seedArchitectWakeRoot(t, pool, fixture)
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(failed, record.SuperviseRequest{Op: "start", Tree: failed, Role: claim.RoleArchitect,
			Generation: architectWakeGeneration, Task: seededArchitectTask(failed), Entry: seededEntry}, before)
		if err != nil {
			t.Fatalf("build the architect start of %s: %v", failed, err)
		}
		if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
			t.Fatalf("queue the architect start of %s: %v", failed, err)
		}
	})
	// The store dates a queued start in its own clock (record.QueuedSupervise.Age), so each start
	// is dated by how long ago it was queued against that clock's now.
	dateStarts := func(root string, age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `update outbox set created_at = now() - make_interval(secs => $1) where issue = $2 and kind = 'supervise'`,
			age.Seconds(), root); err != nil {
			t.Fatalf("date the architect starts of %s %s before now: %v", root, age, err)
		}
	}
	dateStarts(failed, fixedNow.Sub(before))

	pause := pauseTestProject(t, pool, began)
	now = ended.Add(-time.Minute)
	for _, root := range roots {
		if arms := architectStallArmsOf(t, pool, admission, root); len(arms) != 0 {
			t.Errorf("the state's reading of %s while the project is paused names %v, want no arm: the pause stopped its architect", root, arms)
		}
	}

	endTestPause(t, pool, pause, ended)
	now = ended
	if _, err := intake.ApplyFact(ctx, pool, "pause", "project-resumed", intake.ProjectResumed{}, engineStub{}, admission); err != nil {
		t.Fatalf("apply the resume's pass: %v", err)
	}
	for _, root := range roots {
		if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
			t.Fatalf("the architect wakes of %s after the resume's pass = %q, want none: the pause held the project for that time", root, reasons)
		}
		if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); len(marks) != 0 {
			t.Fatalf("the arms %s is marked told for after the resume's pass = %v, want none: a mark spent now would keep a real stall "+
				"of the run from waking anybody", root, marks)
		}
	}

	floor := fmt.Sprintf("counted from the end of pause %d at %s", pause, ended.UTC().Format(time.RFC3339))
	floored := func(root string, arms []record.ArchitectStallArm, why string) {
		t.Helper()
		assertArchitectWakes(t, pool, root, arms, why)
		if reason := architectWakes(t, pool, root)[len(arms)-1]; !strings.Contains(reason, floor) {
			t.Errorf("the wake of %s = %q, want it to say it was %s", root, reason, floor)
		}
	}

	// Eleven minutes after the pause's end, in both clocks: the start the resume's pass wrote
	// for unclaimed has stood unrun that long too.
	now = ended.Add(pastTheHandshake)
	dateStarts(unclaimed, pastTheHandshake)
	applySweep(t, pool, admission, "after-the-pause-1")
	floored(unsent, []record.ArchitectStallArm{record.ArchitectTookNothing},
		"the task of its entry has stood unsent past the bound since the pause ended")
	floored(unclaimed, []record.ArchitectStallArm{record.ArchitectClaimUnreachable},
		"the work of its entry has stood with no claim past the bound since the pause ended")
	assertArchitectWakes(t, pool, failed, []record.ArchitectStallArm{record.ArchitectClaimUnreachable},
		"the start the pause held has stood unrun past the bound since the pause ended")
	if reasons := architectWakes(t, pool, slow); len(reasons) != 0 {
		t.Fatalf("the architect wakes of %s eleven minutes after the pause = %q, want none: the gate's bound is 90 minutes", slow, reasons)
	}

	now = ended.Add(pastTheGate)
	applySweep(t, pool, admission, "after-the-pause-2")
	floored(slow, []record.ArchitectStallArm{record.ArchitectSlowBeforeGate},
		"it has stood at admitted with no gate step past the gate's bound since the pause ended")
	for _, root := range []string{unsent, unclaimed, failed} {
		if reasons := architectWakes(t, pool, root); len(reasons) != 1 {
			t.Fatalf("the architect wakes of %s = %q, want the one told already", root, reasons)
		}
	}
}

// A paused project owes no entry work (decision 11 of dispatch://INFRA-3759): the pass promotes
// nothing while the pause stands (promoteHolds), so a fact the entry sweep's gate let through
// would do nothing, and the gate would let one through a minute for as long as the pause lasts.
// The gate opens for the same owed root once the pause has ended, which the resume's own pass
// and the sweep after it serve.
func TestTheEntrySweepsGateOwesNothingWhileTheProjectIsPaused(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "LEGION-870"
	seedArchitectSweepRoot(t, pool, architectSweepRoot(root))
	if !entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s, whose architect is owed its entry, answers no while the project runs", root)
	}

	pause := pauseTestProject(t, pool, fixedNow)
	if entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s answers yes while the project is paused: the sweep would apply a fact a minute that the "+
			"pass drops at once", root)
	}

	endTestPause(t, pool, pause, fixedNow.Add(time.Minute))
	if !entrySweepGateOpens(t, pool, admission) {
		t.Fatalf("the gate over %s answers no once the pause has ended: its architect is still owed its entry", root)
	}
}

// The stalled-tree notice counts no time a pause held the project, as the architect-stalled arms
// count none (stallFloor, decision 14 of dispatch://INFRA-3759): a pause stops every role of a
// tree, so a tree quiet only because the pause held it is no tree that has stopped. A tree quiet
// past the bound, whose quiet stretch a pause covered, is told nothing at the first pass after
// the resume; it is told once the bound has passed again since the pause ended, and the notice
// still says how long the tree has been quiet, the pause's time included.
func TestTheStalledTreeNoticeCountsNoTimeAPauseHeldTheProject(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	admission.UseStalledTreeNotice(6 * time.Hour)
	registerController(t, pool)
	const root = "LEGION-880"
	seedQuietRoot(t, pool, root, "Q", phase.Implementing)
	seedClaim(t, claimStore(t, pool), root, root, claim.RoleImplementer, supervise.StateIdle)
	settled := settleMovements(t, pool, root)

	pause := pauseTestProject(t, pool, settled.Add(30*time.Minute))
	admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
	tick(t, pool, admission, "while-paused")
	if got := stalledNotices(t, pool); len(got) != 0 {
		t.Fatalf("the controller was told %#v while the project is paused, want nothing: the pause stopped every role of the tree", got)
	}
	resumed := settled.Add(7*time.Hour + time.Minute)
	endTestPause(t, pool, pause, resumed)

	admission.now = func() time.Time { return resumed.Add(time.Minute) }
	tick(t, pool, admission, "after-the-resume")
	if got := stalledNotices(t, pool); len(got) != 0 {
		t.Fatalf("the controller was told %#v at the first pass after the resume, want nothing: the pause held the project "+
			"for most of the tree's quiet time", got)
	}

	admission.now = func() time.Time { return resumed.Add(6*time.Hour + time.Minute) }
	tick(t, pool, admission, "past-the-bound-since-the-resume")
	got := stalledNotices(t, pool)
	if len(got) != 1 || got[0].issue != root || !strings.Contains(got[0].reason, "The tree has been quiet for 13.0 hours.") {
		t.Fatalf("the controller was told %#v once the bound had passed since the pause ended, want one notice of %s that "+
			"says how long the tree has been quiet", got, root)
	}
}
