package daemon

import (
	"context"
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
