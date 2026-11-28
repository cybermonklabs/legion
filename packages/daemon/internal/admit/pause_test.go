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
	"github.com/sjawhar/legion/daemon/internal/pods"
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
// dispatch://INFRA-3759): the pass rule (Admission.Apply) runs the pass on it whatever it wrote,
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

// The took-nothing arm counts no time before the architect's let-out from the wake pacing. The
// resume lets each held launch out through the pacing, one every 7.5 seconds at the default 8 a
// minute with a burst of 1, so an architect about ninety launches back is let out about eleven
// minutes after the pause's end, and its claim stands launching until then (supervise's
// waitOnMachine). An agent that is not up can take no task, so the arm counts nothing while the
// claim is queued, launching or registered, and counts the task of an agent that is up from the
// later of the task's first queueing and that agent's last ready. A wave of wakes no pause held
// queues launches the same way, and the rule is the same for it.
//
// Each case reads the arms at the pause's end plus 10m30s, when the pacing lets out the 85th
// launch, past the arm's ten minutes from the pause's end:
//
//   - a launching claim whose task was first queued before the pause and whose agent was last
//     ready before it: no arm, and the passes write no wake and no mark;
//   - a launching claim of a root the resume's own pass admitted, whose start queued the task
//     five seconds after the pause's end: no arm, no wake, no mark;
//   - a ready claim whose agent was ready nine minutes ago: no arm, since that agent has been
//     able to take the task for less than the bound;
//   - a ready claim whose agent was ready eleven minutes ago: the arm, and its reason names that
//     ready.
func TestTheTookNothingArmCountsNoTimeBeforeTheArchitectsLetOut(t *testing.T) {
	began, ended := fixedNow.Add(-2*time.Hour), fixedNow
	before := began.Add(-time.Minute)
	now := ended.Add(10*time.Minute + 30*time.Second)
	for _, tc := range []struct {
		name string
		// admitted is when the root was admitted and its entry raised, queued when the task of
		// that entry was first queued to the architect's claim, which stands in state, its agent
		// last ready at lastReady, none where that is the zero time.
		admitted, queued, lastReady time.Time
		state                       supervise.ClaimState
		want                        []record.ArchitectStallArm
		why                         string
	}{
		{
			name: "a launching claim whose task was queued before the pause", admitted: before, queued: before,
			state: supervise.StateLaunching, lastReady: began.Add(-5 * time.Minute),
			why: "the pacing has not let the architect's launch out, so no agent is up to take the task",
		},
		{
			name: "a launching claim whose task the resume's pass queued", admitted: ended, queued: ended.Add(5 * time.Second),
			state: supervise.StateLaunching,
			why:   "the start the resume's pass wrote queued the task while the launch waits in the pacing, so no agent is up to take it",
		},
		{
			name: "a ready claim whose agent was ready inside the bound", admitted: before, queued: before,
			state: supervise.StateReady, lastReady: now.Add(-9 * time.Minute),
			why: "the agent has been able to take the task for less than the bound",
		},
		{
			name: "a ready claim whose agent was ready past the bound", admitted: before, queued: before,
			state: supervise.StateReady, lastReady: now.Add(-11 * time.Minute), want: []record.ArchitectStallArm{record.ArchitectTookNothing},
			why: "the agent has been able to take the task for more than the bound and no turn confirmed it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			admission.now = func() time.Time { return now }
			endTestPause(t, pool, pauseTestProject(t, pool, began), ended)
			const root = "LEGION-880"
			fixture := architectWakeRoot(root)
			fixture.registered, fixture.state, fixture.lastReady = true, tc.state, tc.lastReady
			fixture.admittedAt, fixture.raisedAt, fixture.queuedAt, fixture.confirmed = tc.admitted, tc.admitted, tc.queued, false
			seedArchitectWakeRoot(t, pool, fixture)

			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, tc.want) {
				t.Fatalf("the state's reading of %s at the pause's end + 10m30s names the arms %v, want %v: %s", root, arms, tc.want, tc.why)
			}
			applySweep(t, pool, admission, "let-out-1")
			applySweep(t, pool, admission, "let-out-2")
			assertArchitectWakes(t, pool, root, tc.want, tc.why)
			if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); !slices.Equal(marks, tc.want) {
				t.Fatalf("the arms %s is marked told for = %v, want %v: %s", root, marks, tc.want, tc.why)
			}
			if len(tc.want) == 0 {
				return
			}
			if ready := tc.lastReady.UTC().Format(time.RFC3339); !strings.Contains(architectWakes(t, pool, root)[0], ready) {
				t.Fatalf("the wake of %s = %q, want it to name %s, the agent's ready the arm counted from", root, architectWakes(t, pool, root)[0], ready)
			}
		})
	}
}

// The slow-before-gate arm counts no time before its architect's let-out from the wake pacing once
// the architect has confirmed a task of its entry. A root the resume's own pass admits, and a root
// whose architect the pause stopped owing a launch, wait for that launch in the pacing, about
// eleven minutes for an architect about ninety launches back, and that wait is not the
// architect's. So the arm counts its 90 minutes from the latest of the root's admission or raised
// entry, the end of the latest pause, the let-out of the launch the pause held for the architect
// (the stuck clock of that pause's resume of it, pod_resumes.clock_at), and the moment the
// architect first confirmed a task of the root's entry (the claim's handshake), where it has
// confirmed one:
//
//   - a root admitted at the pause's end, whose architect stood launching until 11m30s after it,
//     was ready then and confirmed its entry's task at 12 minutes: no arm 91 minutes after the
//     pause's end, and the arm 91 minutes after the confirmation, naming it;
//   - a root whose architect confirmed its entry before the pause and was stopped by it owing a
//     launch, whose resume of that pause was let out 11 minutes after its end: no arm 91 minutes
//     after the pause's end, and the arm 91 minutes after the let-out, naming it.
func TestTheGateArmCountsNoTimeBeforeTheArchitectsLetOut(t *testing.T) {
	began, ended := fixedNow.Add(-2*time.Hour), fixedNow
	ready := ended.Add(11*time.Minute + 30*time.Second)
	for _, tc := range []struct {
		name   string
		change func(*architectWakeFixture)
		// queued and confirmed are the claim's handshake of the root's entry: when a task of it
		// was first queued to the architect's claim and when a turn first confirmed one.
		queued, confirmed time.Time
		// letOut is the stuck clock of the architect's resume the pause held, none where it is
		// the zero time, and from the moment the arm counts from, which its reason names.
		letOut, from time.Time
		why          string
	}{
		{
			name: "a root the resume's pass admitted",
			change: func(f *architectWakeFixture) {
				f.admittedAt, f.raisedAt, f.state, f.lastReady, f.task = ended, ended, supervise.StateIdle, ready, false
			},
			queued: ended.Add(5 * time.Second), confirmed: ended.Add(12 * time.Minute),
			from: ended.Add(12 * time.Minute),
			why:  "the architect could take no turn of its entry before the pacing let its launch out, and it confirmed the entry's task then",
		},
		{
			name: "an architect the pause stopped owing a launch",
			change: func(f *architectWakeFixture) {
				f.admittedAt, f.raisedAt = began.Add(-time.Hour), began.Add(-time.Hour)
				f.state, f.lastReady, f.task = supervise.StateWorking, ready, false
			},
			queued: began.Add(-time.Hour), confirmed: began.Add(-time.Hour + time.Minute),
			letOut: ended.Add(11 * time.Minute), from: ended.Add(11 * time.Minute),
			why: "the architect the pause stopped could take no turn before the pacing let out the launch the pause owed it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			registerController(t, pool)
			admission := quiet(t, 1)
			now := ended.Add(91 * time.Minute)
			admission.now = func() time.Time { return now }
			pause := pauseTestProject(t, pool, began)
			endTestPause(t, pool, pause, ended)
			const root = "LEGION-881"
			fixture := architectWakeRoot(root)
			tc.change(&fixture)
			seedArchitectWakeRoot(t, pool, fixture)
			putArchitectHandshake(t, pool, architectToken(t, root), seededEntry, architectWakeGeneration, tc.queued, tc.confirmed)
			if !tc.letOut.IsZero() {
				// The resume the pause's stop took for the launch it owed the architect
				// (store.PodRecords.TakePodWake), whose stuck clock the reservation of that
				// launch moved to its let-out (store.Store.SetResumeClock).
				project, err := claim.ProjectToken(testProject)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `insert into pod_resumes (release_id, project, issue, role, kind, at, clock_at, pause_id)
					values (0, $1, $2, $3, $4, $5, $6, $7)`, project, root, string(claim.RoleArchitect), pods.WakePause,
					began.Add(time.Minute), tc.letOut, pause); err != nil {
					t.Fatalf("record the architect's resume pause %d held: %v", pause, err)
				}
			}

			if arms := architectStallArmsOf(t, pool, admission, root); len(arms) != 0 {
				t.Fatalf("the state's reading of %s 91 minutes after the pause's end names the arms %v, want none: %s", root, arms, tc.why)
			}
			applySweep(t, pool, admission, "gate-let-out-1")
			if reasons := architectWakes(t, pool, root); len(reasons) != 0 {
				t.Fatalf("the architect wakes of %s 91 minutes after the pause's end = %q, want none: %s", root, reasons, tc.why)
			}
			if marks := architectWakeMarks(t, pool, root, architectWakeGeneration); len(marks) != 0 {
				t.Fatalf("the arms %s is marked told for 91 minutes after the pause's end = %v, want none: %s", root, marks, tc.why)
			}

			now = tc.from.Add(91 * time.Minute)
			want := []record.ArchitectStallArm{record.ArchitectSlowBeforeGate}
			if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
				t.Fatalf("the state's reading of %s 91 minutes after %s names the arms %v, want %v: %s", root, tc.from, arms, want, tc.why)
			}
			applySweep(t, pool, admission, "gate-let-out-2")
			assertArchitectWakes(t, pool, root, want, tc.why)
			if from := tc.from.UTC().Format(time.RFC3339); !strings.Contains(architectWakes(t, pool, root)[0], from) {
				t.Fatalf("the wake of %s = %q, want it to name %s, the moment the arm counted from", root, architectWakes(t, pool, root)[0], from)
			}
		})
	}
}

// The slow-before-gate arm still tells of a root whose architect has confirmed no task of its
// entry. It then counts from the later of the root's admission and raised entry, as it did before
// it read the confirmation, and from no earlier than the end of a pause or the let-out of a
// launch a pause held. The wake pacing can feed it only while the entry is unconfirmed, and 90
// minutes with no first confirmation says something else is wrong: a start that never runs, a
// task of no entry held ahead of the entry's, or an architect that took nothing. Here no pause
// was ever made, the root stands at admitted with no step of its design gate registered 91
// minutes after its admission and entry, and its architect's claim is idle with no handshake of
// that entry, because the start that would queue the entry's task to it never ran: the arm
// names it.
func TestTheGateArmTellsOfAnEntryNoTurnOfTheArchitectConfirmed(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	registerController(t, pool)
	admission := quiet(t, 1)
	const root = "LEGION-882"
	since := fixedNow.Add(-91 * time.Minute)
	fixture := architectWakeRoot(root)
	fixture.admittedAt, fixture.raisedAt, fixture.given, fixture.task = since, since, 0, false
	seedArchitectWakeRoot(t, pool, fixture)
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(root, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
			Generation: architectWakeGeneration, Task: seededArchitectTask(root), Entry: seededEntry}, since)
		if err != nil {
			t.Fatalf("build the architect start of %s: %v", root, err)
		}
		if err := record.NewStore().Enqueue(ctx, tx, row); err != nil {
			t.Fatalf("queue the architect start of %s: %v", root, err)
		}
	})

	want := []record.ArchitectStallArm{record.ArchitectSlowBeforeGate}
	why := "no turn of the architect has confirmed a task of the root's entry in the 91 minutes since its admission, which the wake pacing does not explain"
	if arms := architectStallArmsOf(t, pool, admission, root); !slices.Equal(arms, want) {
		t.Fatalf("the state's reading of %s names the arms %v, want %v: %s", root, arms, want, why)
	}
	applySweep(t, pool, admission, "unconfirmed-gate")
	assertArchitectWakes(t, pool, root, want, why)
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
