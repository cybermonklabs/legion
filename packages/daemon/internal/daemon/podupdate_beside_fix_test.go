package daemon

// An update round's ask beside a fix: each role is told where it pushes (supervise.UpdatePush), a
// step beside the fix to a branch of its own, the fix to its own branch F, and the implementer
// nowhere while its fix reads its job. The sweep reads the pushes once for the issue through the
// workflow's engine (supervision.updatePushes, workflow.(*Engine).UpdatePushes) and copies each
// role's push onto its row (record.UpdateAsk), so these tests read the rows' typed fields and no
// text. They drive the pod-update rig of podupdate_test.go, whose supervision reads the pushes as
// the workflow's attach wires it.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// besideX is the head of the task's pull request when fix 1 starts, and besideY the head the
	// fix ends at.
	besideX = "c0de0000000000000000000000000000000000a1"
	besideY = "c0de0000000000000000000000000000000000b2"
)

// besideRoles are the four ready claims of the issue's pod these tests ask.
var besideRoles = []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleArchitect}

// newBesideRig is the pod-update rig with the ready claims of besideRoles.
func newBesideRig(t *testing.T) *podUpdateRig {
	t.Helper()
	workers := make([]podUpdateRole, len(besideRoles))
	for i, role := range besideRoles {
		workers[i] = readyWorker(role)
	}
	return newPodUpdateRig(t, workers...)
}

// besideBranch is the branch beside fix 1 of task a of the rig's issue that role's step pushes to
// (claim.BesideBranch), which claim.IsBesideBranch accepts.
func besideBranch(t *testing.T, role claim.Role) string {
	t.Helper()
	branch, err := claim.BesideBranch(podUpdateIssue, 1, "a", 1, role)
	if err != nil || !claim.IsBesideBranch(branch) {
		t.Fatalf("BesideBranch(%s, 1, a, 1, %s) = %q, %v; want a branch beside the fix", podUpdateIssue, role, branch, err)
	}
	return branch
}

// pushOf is the push an ask's row carries.
func pushOf(ask record.UpdateAsk) supervise.UpdatePush {
	return supervise.UpdatePush{Branch: ask.Branch, OwnFix: ask.OwnFix, FixReads: ask.FixReads}
}

// besidePlace is place of task a, a step of the walk.
func besidePlace(place int) record.StepPlace {
	return record.StepPlace{Task: "a", Kind: record.PlaceStep, Place: place}
}

// inTx runs write in one transaction over the rig's database, failing the test on its error.
func (r *podUpdateRig) inTx(what string, write func(ctx context.Context, tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, write); err != nil {
		r.t.Fatalf("%s: %v", what, err)
	}
}

// seedBesideTask writes the plan of the rig's issue: task a, which fixes its CI, with the steps
// implementer, tester, reviewer and the owner's acceptance, standing at place in phase p, and its
// pull request open at besideX.
func (r *podUpdateRig) seedBesideTask(place int, p phase.Phase) {
	r.t.Helper()
	r.movesTo(p)
	r.inTx("seed task a", func(ctx context.Context, tx pgx.Tx) error {
		v := record.PlanVersion{Issue: podUpdateIssue, Generation: 1, Version: 1, ArtifactID: "plan-" + podUpdateIssue,
			Source: record.PlanSourceOwner, GivenBy: "owner", GivenAt: podUpdateStart.Add(-time.Hour)}
		tasks := []record.PlanTask{{Issue: podUpdateIssue, Generation: 1, Task: "a", PlanVersion: 1, BlockText: "Task a.",
			State: record.TaskWaiting, FixesCI: true}}
		steps := []record.PlanStep{
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 1, Role: "implementer", Type: "implementation"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 2, Role: "tester", Type: "testing"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 3, Role: "reviewer", Type: "review"},
			{Issue: podUpdateIssue, Generation: 1, Task: "a", Position: 4, Role: "acceptance", AgentKey: "owner", Type: "review"},
		}
		if err := r.records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		if err := r.records.MoveTask(ctx, tx, podUpdateIssue, 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: place}, record.TaskRunning); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: podUpdateIssue,
			Repo: "acme/widgets", Number: 42, Branch: claim.IssueBranch(podUpdateIssue), HeadSHA: besideX, Failing: []string{}})
	})
}

// openFix writes fix 1 of task a for the check k-ci failed at besideX, at the implementer's place:
// reading its failed job, or running at the implementer's entry 2.
func (r *podUpdateRig) openFix(state record.FixState) {
	r.t.Helper()
	r.inTx("open fix 1", func(ctx context.Context, tx pgx.Tx) error {
		fix := record.TaskFix{Issue: podUpdateIssue, Generation: 1, Task: "a", Fix: 1, State: record.FixReading,
			Check: "k-ci", Head: besideX, Place: 1}
		if err := r.records.PutFix(ctx, tx, fix); err != nil || state == record.FixReading {
			return err
		}
		started := podUpdateStart.Add(-10 * time.Minute)
		fix.State, fix.Entry, fix.StartedAt = record.FixRunning, 2, &started
		return r.records.UpdateFix(ctx, tx, fix)
	})
}

// seededStart counts one start of place for role's claim with the store's writers, open, and, where
// branch is not "", binds it beside fix 1 to branch (MarkStepBesideFix, SetStepPushBranch).
func (r *podUpdateRig) seededStart(place int, step, typ string, role claim.Role, branch string) {
	r.t.Helper()
	at := besidePlace(place)
	started := podUpdateStart.Add(-5 * time.Minute)
	r.inTx("count a start of "+step, func(ctx context.Context, tx pgx.Tx) error {
		var starts int64
		if _, err := r.records.RaiseStepStarts(ctx, tx, podUpdateIssue, at, func(count int64) record.StepRecord {
			starts = count
			return record.StepRecord{ID: workflow.StepID(podUpdateIssue, 1, at, count), Task: "a", Step: place, Kind: record.PlaceStep,
				Role: step, Type: typ, Agent: string(r.tokens[role]), Generation: 1, StartedAt: &started}
		}); err != nil || branch == "" {
			return err
		}
		if err := r.records.MarkStepBesideFix(ctx, tx, podUpdateIssue, at, starts, 1); err != nil {
			return err
		}
		return r.records.SetStepPushBranch(ctx, tx, podUpdateIssue, at, starts, branch)
	})
}

// askedPushes opens the update round over the rig's pod and runs the sweep that asks, and answers
// each row the sweep wrote by its role. It fails where a role of roles is asked other than once,
// so an ask that wrote no row is no pass.
func (r *podUpdateRig) askedPushes(roles ...claim.Role) map[claim.Role]record.UpdateAsk {
	r.t.Helper()
	r.drifts(podUpdateDrift)
	r.tick()
	for _, role := range roles {
		r.awaitRound(role, podUpdateRound)
	}
	r.clock.advance(time.Minute)
	r.tick()
	asks := map[claim.Role]record.UpdateAsk{}
	counted := map[claim.Role]int{}
	for _, ask := range r.asks() {
		asks[ask.Role] = ask
		counted[ask.Role]++
	}
	for _, role := range roles {
		if counted[role] != 1 {
			r.t.Fatalf("the sweep asked the %s %d times, want once; it asked %v", role, counted[role], counted)
		}
	}
	return asks
}

// wantPushes holds each row of asks to the push want names for its role, and each branch it names
// to a branch beside a fix, which no reader takes for the issue's or a task's branch.
func wantPushes(t *testing.T, asks map[claim.Role]record.UpdateAsk, want map[claim.Role]supervise.UpdatePush) {
	t.Helper()
	for _, role := range besideRoles {
		got := pushOf(asks[role])
		if got != want[role] {
			t.Errorf("the ask to the %s carries %+v, want %+v", role, got, want[role])
		}
		if got.Branch != "" && !claim.IsBesideBranch(got.Branch) {
			t.Errorf("the ask to the %s names the branch %s, want a branch beside the fix", role, got.Branch)
		}
	}
}

// An update round over the pod of an issue in reviewing, where fix 1 of its task runs beside the
// review: the implementer's row holds the fix's own branch F with OwnFix, the tester's and the
// owner's rows each hold that step's own branch beside the fix, and the reviewer's, which pushes
// nothing, holds none. While fix 1 still reads its failed job, the implementer's row holds FixReads
// and no branch, since neither the fix's copy nor F exists yet, and every other row is as it was.
//
// The open bound starts of the tester and of the owner are seeded with the store's writers: a
// walk at this base ends the tester's run before the review starts (startStep's settleStart), so
// no walk stands with all three open. TestAnUpdateAfterTheFixEndsKeepsEachOpenStepsBranch drives
// each open step's start through the engine instead.
//
// The controls, kept: the same sweep for an issue with no fix writes each row with the zero push,
// main's ask; and with no read of the pushes (updatePushes nil, a deployment with no workflow)
// every role is still asked, each with the zero push.
func TestAnUpdateRoundBesideARunningFixAsksEachRoleToPushToItsOwnBranch(t *testing.T) {
	t.Parallel()
	fixBranch := besideBranch(t, claim.RoleImplementer)
	tester, owner := besideBranch(t, claim.RoleTester), besideBranch(t, claim.RoleArchitect)
	beside := func(t *testing.T, state record.FixState) *podUpdateRig {
		t.Helper()
		rig := newBesideRig(t)
		rig.seedBesideTask(3, phase.Reviewing)
		rig.openFix(state)
		rig.seededStart(2, "tester", "testing", claim.RoleTester, tester)
		rig.seededStart(3, "reviewer", "review", claim.RoleReviewer, "")
		rig.seededStart(4, "acceptance", "review", claim.RoleArchitect, owner)
		return rig
	}
	others := map[claim.Role]supervise.UpdatePush{
		claim.RoleTester:    {Branch: tester},
		claim.RoleArchitect: {Branch: owner},
		claim.RoleReviewer:  {},
	}
	with := func(implementer supervise.UpdatePush) map[claim.Role]supervise.UpdatePush {
		want := map[claim.Role]supervise.UpdatePush{claim.RoleImplementer: implementer}
		for role, push := range others {
			want[role] = push
		}
		return want
	}

	t.Run("fix 1 runs", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixRunning)
		wantPushes(t, rig.askedPushes(besideRoles...), with(supervise.UpdatePush{Branch: fixBranch, OwnFix: true}))
	})
	t.Run("fix 1 reads its failed job", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixReading)
		wantPushes(t, rig.askedPushes(besideRoles...), with(supervise.UpdatePush{FixReads: true}))
	})
	t.Run("control: an issue with no fix asks every role with the zero push", func(t *testing.T) {
		t.Parallel()
		rig := newBesideRig(t)
		rig.seedBesideTask(3, phase.Reviewing)
		rig.seededStart(3, "reviewer", "review", claim.RoleReviewer, "")
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
	t.Run("control: with no read of the pushes every role is asked with the zero push", func(t *testing.T) {
		t.Parallel()
		rig := beside(t, record.FixRunning)
		rig.s.updatePushes = nil
		wantPushes(t, rig.askedPushes(besideRoles...), map[claim.Role]supervise.UpdatePush{})
	})
}
