package daemon

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// mergedNeedRig is a rig of a plan one of whose needs merged (seedMergedNeed): the executor of its
// task_branch rows, the issue, and the commits the fake GitHub's repository holds.
type mergedNeedRig struct {
	*taskBranchRig
	issue record.Issue
	// a1 is the head of task a's branch, and m1 the squash of task b's work on main.
	a1, m1 string
}

// seedMergedNeed is the rig of a plan whose task b merged, through the outbox: main stands at m0,
// then task a's branch at a1, which adds a.txt, and task b's branch at b1, which adds b.txt. a is
// done at its last step, and its work did not merge. b is done at its merge: its pull request merged
// by squash into main as m1, and GitHub deleted its branch, as delete_branch_on_merge does. c needs
// b, d needs a and b, e needs a, and z needs c, d and e, so z alone merges, and d, which needs two,
// has a join branch. Each task runs one implementer step, keyed by its id. Where conflict, a's and
// b's commits each add x.txt with other text, so the merge of main into d's join branch conflicts.
// The first step of the work (the gate's opening), applied through the real workflow engine, starts
// c, d and e, whose needs are done, and queues the task_branch row of each.
func seedMergedNeed(t *testing.T, conflict bool) *mergedNeedRig {
	t.Helper()
	ctx := context.Background()
	rig := &taskBranchRig{pool: isolatedOutboxPool(t), records: record.NewStore(), github: githubtest.GitRepository(t, "acme", "widgets", "main")}
	api := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	engine := workflow.New(rig.records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	rig.runner = &outbox{log: quietLogger(), pool: rig.pool, dispatchProject: "LEGION", records: rig.records, tokens: roleTokens{}, project: "legion",
		githubAPI: api, handlers: []intake.Handler{engine}, now: time.Now}
	issue := taskBranchIssue
	issue.Phase = phase.Admitted
	putOutboxIssue(t, rig.pool, rig.records, issue)
	v := record.PlanVersion{Issue: issue.Key, Generation: issue.Generation, Version: 1, ArtifactID: "plan-artifact", Source: record.PlanSourceOwner,
		GivenBy: "legion-legion-legion-208-architect", GivenAt: time.Now()}
	task := func(id string, state record.TaskState, place record.TaskPlace, needs ...string) record.PlanTask {
		return record.PlanTask{Issue: issue.Key, Generation: issue.Generation, Task: id, PlanVersion: 1, BlockText: "Task " + id + ".", Needs: needs,
			State: state, Place: place}
	}
	rows := []record.PlanTask{
		task("a", record.TaskDone, record.TaskPlace{Kind: record.PlaceStep, Position: 1}),
		task("b", record.TaskDone, record.TaskPlace{Kind: record.PlaceMerge}),
		task("c", record.TaskWaiting, record.TaskPlace{}, "b"),
		task("d", record.TaskWaiting, record.TaskPlace{}, "a", "b"),
		task("e", record.TaskWaiting, record.TaskPlace{}, "a"),
		task("z", record.TaskWaiting, record.TaskPlace{}, "c", "d", "e"),
	}
	var steps []record.PlanStep
	for _, row := range rows {
		steps = append(steps, record.PlanStep{Issue: issue.Key, Generation: issue.Generation, Task: row.Task, Position: 1, Role: "implementer",
			Type: "implementation", AgentKey: row.Task})
	}
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		if err := rig.records.PutPlan(ctx, tx, v, rows, steps); err != nil {
			return err
		}
		for _, id := range []string{"a", "b"} {
			if _, err := rig.records.AssignTaskBranch(ctx, tx, issue.Key, issue.Generation, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store the plan of %s: %v", issue.Key, err)
	}
	aFiles, bFiles := map[string]string{"a.txt": "a\n"}, map[string]string{"b.txt": "b\n"}
	if conflict {
		aFiles["x.txt"], bFiles["x.txt"] = "from a\n", "from b\n"
	}
	seeded := &mergedNeedRig{taskBranchRig: rig, issue: issue}
	seeded.a1 = rig.github.Commit(t, rig.branch(t, "a"), aFiles, "a")
	b := rig.branch(t, "b")
	rig.github.Commit(t, b, bFiles, "b")
	seeded.m1 = rig.github.Commit(t, "main", bFiles, "LEGION-208: b (#2)")
	fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), "git", "update-ref", "-d", "refs/heads/"+b)

	factCtx, committed := intake.WithCommitHooks(ctx)
	factCtx = engine.OpenFact(factCtx)
	if err := pgx.BeginFunc(factCtx, rig.pool, func(tx pgx.Tx) error {
		stored, err := rig.records.Issue(factCtx, tx, issue.Key)
		if err != nil {
			return err
		}
		if err := engine.StartFirstPhase(factCtx, tx, *stored, nil); err != nil {
			return err
		}
		return engine.SettleFact(factCtx, tx)
	}); err != nil {
		t.Fatalf("open the gate of %s: %v", issue.Key, err)
	}
	committed()
	return seeded
}

// runJoin claims and runs the task_branch row of task id that the gate's opening queued, and
// answers what the row answered.
func (rig *mergedNeedRig) runJoin(t *testing.T, id string) error {
	t.Helper()
	var row int64
	if err := rig.pool.QueryRow(context.Background(), `select id from outbox where issue = $1 and kind = 'task_branch' and payload->>'task' = $2`,
		rig.issue.Key, id).Scan(&row); err != nil {
		t.Fatalf("read the task_branch row of task %s: %v", id, err)
	}
	return rig.attemptAt(t, row, time.Now().UTC())
}

// git is the output of git with args in the fake's repository.
func (rig *mergedNeedRig) git(t *testing.T, args ...string) string {
	t.Helper()
	return fixturegit.Isolated(t, t.TempDir()).Run(rig.github.Dir(), append([]string{"git"}, args...)...)
}

// A task whose need merged starts from main, which holds that need's work, and never reads the
// need's branch, which GitHub deleted at the merge (seedMergedNeed). c, which needs b alone, has
// its branch made at m1, with no merge commit. d, which needs a and b, has its join branch made at
// a1, then main merged into it once, with the commit message `LEGION-208: join main, which holds
// the work of task b`; d's branch starts at the join branch's head, whose tree holds a's and b's
// files. e, which needs a alone, starts at a1. Where the merge of main into d's join branch
// conflicts, the conflict is recorded under b, whose work main holds, the join branch stands on a's
// work, and d's first step is told to join main@origin there, not b's deleted branch. Where GitHub
// has no main, c's row waits for it, and its owner is told once, naming main and b.
//
// The faults it catches are a join that reads a merged need's branch, so the task's branch is never
// made (its control: StartsFrom read as the needs' branches, as the executor read them, under which
// c's row is answered 404 for b's branch and fails), and a conflict whose repair names that branch
// (its control: the told branch of each conflict read from the need's row, under which d is told
// b's branch).
func TestATaskWhoseNeedMergedStartsFromMain(t *testing.T) {
	t.Parallel()
	t.Run("c needs b alone", func(t *testing.T) {
		t.Parallel()
		rig := seedMergedNeed(t, false)
		if err := rig.runJoin(t, "c"); err != nil {
			t.Fatalf("c's task_branch row answered %v, want its join to end", err)
		}
		branch := rig.branch(t, "c")
		if head := rig.github.Head(t, branch); head != rig.m1 {
			t.Fatalf("task c's branch %s stands at %q, want main's squash of b's work %s, with no merge commit", branch, head, rig.m1)
		}
		if c := rig.task(t, "c"); c.StepWait != record.StepWaitNone || len(c.JoinConflicts) != 0 {
			t.Fatalf("task c after its row ran waits for %q with join conflicts %v, want nothing and none", c.StepWait, c.JoinConflicts)
		}
	})
	t.Run("d needs a and b", func(t *testing.T) {
		t.Parallel()
		rig := seedMergedNeed(t, false)
		if err := rig.runJoin(t, "d"); err != nil {
			t.Fatalf("d's task_branch row answered %v, want its join to end", err)
		}
		join := rig.github.Head(t, joinBranchOfD)
		if parents := rig.parents(t, join); !slices.Equal(parents, []string{rig.a1, rig.m1}) {
			t.Fatalf("the parents of d's join branch head %q = %v, want a's head %s and main's %s", join, parents, rig.a1, rig.m1)
		}
		if message, want := rig.git(t, "log", "-1", "--format=%s", join), "LEGION-208: join main, which holds the work of task b"; message != want {
			t.Fatalf("the message of d's join of main = %q, want %q", message, want)
		}
		if head := rig.github.Head(t, rig.branch(t, "d")); head != join {
			t.Fatalf("task d's branch stands at %q, want its join branch's head %s", head, join)
		}
		if files := strings.Fields(rig.git(t, "ls-tree", "-r", "--name-only", join)); !slices.Contains(files, "a.txt") || !slices.Contains(files, "b.txt") {
			t.Fatalf("the tree of d's join holds %v, want a.txt and b.txt", files)
		}
		if d := rig.task(t, "d"); d.StepWait != record.StepWaitNone || len(d.JoinConflicts) != 0 || d.JoinUnmerged {
			t.Fatalf("task d after its row ran waits for %q with join conflicts %v and unmerged %t, want nothing", d.StepWait, d.JoinConflicts, d.JoinUnmerged)
		}
	})
	t.Run("e needs a alone", func(t *testing.T) {
		t.Parallel()
		rig := seedMergedNeed(t, false)
		if err := rig.runJoin(t, "e"); err != nil {
			t.Fatalf("e's task_branch row answered %v, want its join to end", err)
		}
		if head := rig.github.Head(t, rig.branch(t, "e")); head != rig.a1 {
			t.Fatalf("task e's branch stands at %q, want a's head %s", head, rig.a1)
		}
	})
	t.Run("the merge of main into d's join branch conflicts", func(t *testing.T) {
		t.Parallel()
		rig := seedMergedNeed(t, true)
		if err := rig.runJoin(t, "d"); err != nil {
			t.Fatalf("d's task_branch row answered %v, want its join to end with a conflict", err)
		}
		if d := rig.task(t, "d"); !slices.Equal(d.JoinConflicts, []string{"b"}) {
			t.Fatalf("task d's join conflicts = %v, want [b], whose work main holds", d.JoinConflicts)
		}
		for _, branch := range []string{joinBranchOfD, rig.branch(t, "d")} {
			if head := rig.github.Head(t, branch); head != rig.a1 {
				t.Fatalf("%s stands at %q, want a's head %s, the work that joined", branch, head, rig.a1)
			}
		}
		task := newestStartTask(t, rig.taskBranchRig, rig.issue)
		repair := "`jj new " + joinBranchOfD + "@origin main@origin -m \"LEGION-208: join the work of task b\"`"
		if b := rig.branch(t, "b") + "@origin"; !strings.Contains(task, repair) || strings.Contains(task, b) {
			t.Fatalf("the task of d's start row = %q,\nwant it to hold %q and no %s", task, repair, b)
		}
	})
	t.Run("GitHub has no main", func(t *testing.T) {
		t.Parallel()
		rig := seedMergedNeed(t, false)
		rig.git(t, "update-ref", "-d", "refs/heads/main")
		if err := rig.runJoin(t, "c"); err == nil || !strings.Contains(err.Error(), "GitHub has no branch main") {
			t.Fatalf("c's task_branch row while GitHub has no main answered %v, want a failure naming main", err)
		}
		told := "architect: task c of LEGION-208 waits for the join of its needs: GitHub has no branch main, which holds the work of its need b. " +
			"Push that branch again, or stop the task with stop_task."
		if notices := rig.joinFailedNotices(t); !reflect.DeepEqual(notices, []string{told}) {
			t.Fatalf("the task-join-failed notices = %q, want the one %q", notices, told)
		}
		if wait := rig.task(t, "c").StepWait; wait != record.StepWaitJoin {
			t.Fatalf("c's step waits for %q, want its join", wait)
		}
	})
}
