package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	stepplan "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// copyIssue is the issue whose working copies each test of this file removes.
var copyIssue = record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
	Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A"}

// copyRig is the executor of the removals of copyIssue's working copies on the daemon's host,
// whose removal records each working copy it is handed (removed) and removes nothing.
type copyRig struct {
	pool    *store.Pool
	records *record.Postgres
	runtime *fake.Runtime
	runner  *outbox
	removed []workspace.Workspace
}

// newCopyRig stores issue and its plan at the issue's generation, one task for each of tasks in
// that order, each given its branch as its start gives it (record.Store.AssignTaskBranch): the
// first takes the issue's own branch, each other legion/<KEY>.<id>.
func newCopyRig(t *testing.T, issue record.Issue, tasks ...string) *copyRig {
	t.Helper()
	ctx := context.Background()
	rig := &copyRig{pool: isolatedOutboxPool(t), records: record.NewStore()}
	putOutboxIssue(t, rig.pool, rig.records, issue)
	if len(tasks) > 0 {
		v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner, GivenAt: time.Now()}
		var rows []record.PlanTask
		var steps []record.PlanStep
		for _, task := range tasks {
			rows = append(rows, record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: task, PlanVersion: 1, BlockText: "Task " + task + ".", State: record.TaskWaiting})
			steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: task, Position: 1, Role: "implementer",
				Type: stepplan.BuiltinRoles()["implementer"].Type})
		}
		if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := rig.records.PutPlan(ctx, tx, v, rows, steps); err != nil {
				return err
			}
			for _, task := range tasks {
				if _, err := rig.records.AssignTaskBranch(ctx, tx, issue.Key, issue.Generation, task); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("store the plan of %s: %v", issue.Key, err)
		}
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	rig.runtime = rt
	rig.runner = &outbox{log: quietLogger(), pool: rig.pool, dispatchProject: "LEGION", records: rig.records, supervisor: sup,
		trees: outboxTreeStore(t, rig.pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: claim.IssueBranch(issue.Key)}, nil
		},
		remove: func(_ context.Context, got workspace.Workspace, _ sync.Locker) error {
			rig.removed = append(rig.removed, got)
			return nil
		},
	}
	return rig
}

// copyOf is the working copy Location names for name on the rig's host: the issue's own for the
// issue's key, a task's for <KEY>.<part>.
func (rig *copyRig) copyOf(t *testing.T, name string) workspace.Workspace {
	t.Helper()
	working, err := workspace.Location(rig.runner.stateDir, ghrepo.MustParse(copyIssue.Repo), name)
	if err != nil {
		t.Fatal(err)
	}
	return working
}

// enqueue writes payload as a row of copyIssue and answers it with the id the outbox gave it.
func (rig *copyRig) enqueue(t *testing.T, issue string, payload record.OutboxPayload) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue, payload, time.Now())
	if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		row.ID, err = rig.records.EnqueueID(context.Background(), tx, row)
		return err
	}); err != nil {
		t.Fatalf("queue the %s row of %s: %v", row.Kind, issue, err)
	}
	return row
}

// The removal of a removed task's working copy removes that working copy and no other: of the plan
// of a, which holds the issue's own branch, and c, on legion/LEGION-208.c, the row that names
// legion/LEGION-208.b, the branch of a task a later version removed, removes the working copy
// <state>/workspaces/acme/widgets/legion-208.b, handed the issue's clone, and neither the issue's
// own working copy nor c's.
//
// The fault it catches is the working copy of a removed task left on the host, and a removal that
// takes another task's, or the issue's own (its control: the removal of the issue's Location in
// place of the task's name, which removes legion-208).
func TestTheRemovalOfARemovedTasksWorkingCopyRemovesThatCopyAlone(t *testing.T) {
	t.Parallel()
	rig := newCopyRig(t, copyIssue, "a", "c")
	row := rig.enqueue(t, copyIssue.Key, record.TaskCopyRemove{Generation: 1, Task: "b", Branch: "legion/LEGION-208.b"})
	if err := rig.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("remove the working copy of b: %v", err)
	}
	if want := []workspace.Workspace{rig.copyOf(t, "LEGION-208.b")}; !reflect.DeepEqual(rig.removed, want) {
		t.Fatalf("the working copies removed = %+v, want b's alone, %+v", rig.removed, want)
	}
	if got := rig.removed[0]; got.Clone != rig.copyOf(t, copyIssue.Key).Clone || filepath.Base(got.Dir) != "legion-208.b" {
		t.Fatalf("the removal of b's working copy was handed %+v, want the directory legion-208.b of the issue's clone", got)
	}
}

// A removal finishes without acting where the branch it names is held by a task of the plan the
// issue runs now: a record of a release before gave a later generation's tasks the names an earlier
// one gave, so the working copy of legion/LEGION-208.b is then a task's of that generation. The
// issue stands at generation 2, whose plan's task b holds legion/LEGION-208.b; the row of
// generation 1's removal of b removes nothing, and so does one on a runtime that provisions each
// claim's workspace in its pod, whose disk the daemon does not reach.
//
// The fault it catches is a late removal that takes the working copy of a live task (its controls:
// the executor without the plan's read, and without the pods' fence; each removes legion-208.b).
func TestTheRemovalOfAWorkingCopyFinishesWithoutActingWhereALiveTaskHoldsItOrAPodHoldsIt(t *testing.T) {
	t.Parallel()
	later := copyIssue
	later.Generation = 2
	rig := newCopyRig(t, later, "a", "b")
	row := rig.enqueue(t, later.Key, record.TaskCopyRemove{Generation: 1, Task: "b", Branch: "legion/LEGION-208.b"})
	if err := rig.runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the late removal of b's working copy = %v, want it finished without acting", err)
	}
	if len(rig.removed) != 0 {
		t.Fatalf("the late removal of b's working copy removed %+v, want nothing: task b of generation 2 holds legion/LEGION-208.b", rig.removed)
	}

	pods := newCopyRig(t, copyIssue, "a")
	pods.runtime.InPod = true
	removal := pods.enqueue(t, copyIssue.Key, record.TaskCopyRemove{Generation: 1, Task: "b", Branch: "legion/LEGION-208.b"})
	if err := pods.runner.execute(context.Background(), removal); err != nil {
		t.Fatalf("the removal of b's working copy under a runtime of pods = %v, want it finished without acting", err)
	}
	if len(pods.removed) != 0 {
		t.Fatalf("the removal of b's working copy under a runtime of pods removed %+v, want nothing", pods.removed)
	}
}

// A removal waits while the process that worked in the working copy may still run: while an older
// step_stop row of the issue, the stop of the removed task's step, is still queued; and while a
// claim of the issue holds a pending delivery on the branch, whose turn works in that copy and whose
// send would make it again. A step_stop queued after the removal, one of another issue and a
// suspend hold nothing back. Once the claim's delivery is retired, the copy goes.
//
// The fault it catches is a working copy removed under the agent that works in it (its controls:
// the executor without the step_stop's read, and without the read of the claims' deliveries; each
// removes legion-208.b while the test expects a wait).
func TestTheRemovalOfAWorkingCopyWaitsForTheStopOfItsStepAndForAnyDeliveryOnItsBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newCopyRig(t, copyIssue, "a")
	stop := record.SuperviseRequest{Op: "step_stop", Tree: copyIssue.Tree, Role: claim.RoleImplementer, Generation: 1, Reason: "task b was stopped"}
	older := rig.enqueue(t, copyIssue.Key, stop)
	rig.enqueue(t, "LEGION-209", stop)
	rig.enqueue(t, copyIssue.Key, record.SuperviseRequest{Op: "suspend", Tree: copyIssue.Tree, Role: claim.RoleTester, Generation: 1})
	row := rig.enqueue(t, copyIssue.Key, record.TaskCopyRemove{Generation: 1, Task: "b", Branch: "legion/LEGION-208.b"})
	rig.enqueue(t, copyIssue.Key, stop)
	if err := rig.runner.execute(ctx, row); !errors.Is(err, wait.ErrWaiting) || len(rig.removed) != 0 {
		t.Fatalf("the removal with an older step_stop of the issue queued = %v (removed %+v), want a wait and nothing removed", err, rig.removed)
	}
	if _, err := rig.pool.Exec(ctx, `delete from outbox where id = $1`, older.ID); err != nil {
		t.Fatalf("finish the older step_stop: %v", err)
	}

	start := mustOutboxRow(t, copyIssue.Key, record.SuperviseRequest{Op: "start", Tree: copyIssue.Tree, Role: claim.RoleImplementer,
		Task: "Review b.", Generation: 1, Branch: "legion/LEGION-208.b"}, time.Now())
	start.ID = 1
	if err := rig.runner.execute(ctx, start); err != nil {
		t.Fatalf("start the implementer on b's branch: %v", err)
	}
	token, err := claim.NewToken("legion", copyIssue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, found := rig.runner.supervisor.Machine(token)
	if !found || machine.Claim().Pending == nil || machine.Claim().Pending.Branch != "legion/LEGION-208.b" {
		t.Fatalf("premise: the implementer's claim holds a pending delivery on legion/LEGION-208.b, found %t", found)
	}
	if err := rig.runner.execute(ctx, row); !errors.Is(err, wait.ErrWaiting) || len(rig.removed) != 0 {
		t.Fatalf("the removal with a delivery pending on b's branch = %v (removed %+v), want a wait and nothing removed", err, rig.removed)
	}

	// The claim's process goes with its tree's close, the expiry of the tree's linger.
	until := time.Now().Add(-time.Minute)
	closing := copyIssue
	closing.Phase, closing.Status, closing.LingerUntil = phase.Done, "done", &until
	putOutboxIssue(t, rig.pool, rig.records, closing)
	if err := rig.runner.execute(ctx, mustOutboxRow(t, copyIssue.Key, record.SuperviseRequest{Op: "tree_close", Tree: copyIssue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Linger: 1}, time.Now())); err != nil {
		t.Fatalf("close the implementer's claim: %v", err)
	}
	if held := machine.Claim(); held.State != supervise.StateRetired {
		t.Fatalf("premise: the implementer's claim is retired by its tree's close, claim %+v", held)
	}
	if err := rig.runner.execute(ctx, row); err != nil {
		t.Fatalf("the removal once the delivery is retired: %v", err)
	}
	if want := []workspace.Workspace{rig.copyOf(t, "LEGION-208.b")}; !reflect.DeepEqual(rig.removed, want) {
		t.Fatalf("the working copies removed = %+v, want b's, %+v", rig.removed, want)
	}
}

// The removal of an issue's workspace at its linger's expiry removes each task's working copy
// beside the issue's own, then the issue's own: legion-208.b, legion-208.c.2, whose aside a removal
// cut short left beside it, and legion-208.d, of which only that aside is left. It removes nothing
// of another issue (legion-2080, legion-2080.b), nothing that is no task's working copy
// (legion-208.join, legion-208.lock), not the aside of the issue's own (legion-208.removing, which
// the issue's own removal clears), and no file or link that bears a task's name (legion-208.e,
// legion-208.f). An issue with no working copy of a task removes its own alone, as before.
//
// The fault it catches is the working copy of each task of an issue left on the host for ever once
// the issue's tree closes (its control: removeWorkspace without the task copies, which removes
// legion-208 alone).
func TestTheRemovalOfAnIssuesWorkspaceRemovesEachTasksWorkingCopy(t *testing.T) {
	t.Parallel()
	until := time.Now().Add(-time.Minute)
	done := copyIssue
	done.Phase, done.Status, done.Generation, done.LingerUntil = phase.Done, "done", 2, &until
	rig := newCopyRig(t, done)
	issueDir := rig.copyOf(t, done.Key).Dir
	beside := filepath.Dir(issueDir)
	for _, dir := range []string{"legion-208", "legion-208.b", "legion-208.c.2", "legion-208.c.2.removing", "legion-208.d.removing",
		"legion-2080", "legion-2080.b", "legion-208.join", "legion-208.lock", "legion-208.removing"} {
		if err := os.MkdirAll(filepath.Join(beside, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(beside, "legion-208.e"), []byte("no working copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(beside, "legion-208.f")); err != nil {
		t.Fatal(err)
	}

	if err := rig.runner.execute(context.Background(), mustOutboxRow(t, done.Key, record.WorkspaceRemove{Linger: done.Generation, Tree: done.Tree}, time.Now())); err != nil {
		t.Fatalf("remove the workspace of %s: %v", done.Key, err)
	}
	var got []string
	for _, removed := range rig.removed {
		got = append(got, filepath.Base(removed.Dir))
	}
	if want := []string{"legion-208.b", "legion-208.c.2", "legion-208.d", "legion-208"}; !slices.Equal(got, want) {
		t.Fatalf("the working copies the removal of %s's workspace removed = %v, want %v", done.Key, got, want)
	}
	for _, removed := range rig.removed[:3] {
		if removed.Clone != rig.copyOf(t, done.Key).Clone {
			t.Fatalf("the working copy %s was removed as a workspace of %s, want the issue's clone", removed.Dir, removed.Clone)
		}
	}

	alone := newCopyRig(t, done)
	if err := alone.runner.execute(context.Background(), mustOutboxRow(t, done.Key, record.WorkspaceRemove{Linger: done.Generation, Tree: done.Tree}, time.Now())); err != nil {
		t.Fatalf("remove the workspace of %s with no working copy of a task: %v", done.Key, err)
	}
	if want := []workspace.Workspace{alone.copyOf(t, done.Key)}; !reflect.DeepEqual(alone.removed, want) {
		t.Fatalf("the working copies removed with no working copy of a task = %+v, want the issue's own alone, %+v", alone.removed, want)
	}
}
