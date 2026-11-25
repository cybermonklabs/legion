package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// exampleStand is where one task of the example plan stands (seedExamplePlan).
type exampleStand struct {
	state record.TaskState
	place record.TaskPlace
}

// seedExamplePlan is stepRoot standing at at, holding the example plan of tasks a to e, each task
// where stands places it: a needs nothing and runs `implementer reviewer`, b needs nothing and runs
// `planner implementer reviewer`, c needs b and d needs a and c, each running `implementer
// reviewer`, and e needs d and runs `implementer tester`, so e is the task that merges. This
// release names one agent of each role for an issue, so the steps of a role share one key and one
// agent in every task. Each task works on its branch as the starts of a plan give them, a on the
// issue's own legion/LEGION-208 and each other task on legion/LEGION-208.<id>, where its own pull
// request is open at head-<id>: #1 for a (which takes the place of stepRoot's #42 on that branch)
// to #5 for e, in task order.
func seedExamplePlan(t *testing.T, pool *pgxpool.Pool, at phase.Phase, stands map[string]exampleStand) record.Issue {
	t.Helper()
	root := stepRoot(t, pool, at)
	task := func(id string, needs []string, steps ...string) planTask {
		agents := make([]claim.Agent, len(steps))
		for i, step := range steps {
			role, key, _ := strings.Cut(step, ":")
			agents[i] = claim.Agent(role + "-" + key)
		}
		return planTask{id: id, steps: steps, agents: agents, needs: needs, state: stands[id].state, place: stands[id].place}
	}
	seedTasks(t, pool, root.Key, root.Generation,
		task("a", nil, "implementer:i", "reviewer:r"),
		task("b", nil, "planner:p", "implementer:i", "reviewer:r"),
		task("c", []string{"b"}, "implementer:i", "reviewer:r"),
		task("d", []string{"a", "c"}, "implementer:i", "reviewer:r"),
		task("e", []string{"d"}, "implementer:i", "tester:t"))
	seedBranches(t, pool, root.Key, "a", "b", "c", "d", "e")
	for number, id := range []string{"a", "b", "c", "d", "e"} {
		seedPR(t, pool, examplePR(root.Key, id, number+1))
	}
	return root
}

// examplePR is the open pull request #number of task id of the example plan of key, on the task's
// branch (seedExamplePlan) at head-<id>.
func examplePR(key, id string, number int) record.PullRequest {
	branch := "legion/" + key
	if id != "a" {
		branch += "." + id
	}
	return record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: number,
		Branch: branch, HeadSHA: "head-" + id, Failing: []string{}}
}

// bootResumes is the task of role's newest start of key after the boot resumes role's claim, which
// the daemon itself suspended level with its entry (booted): the resume task the boot composes on
// the scope of the step role works (ScopeOfStep), which a worker resumed in a fresh session reads
// with no earlier task to read anything from.
func bootResumes(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, role claim.Role) string {
	t.Helper()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	seedRuleSuspendedOwner(t, pool, project, key, role)
	mustSpend(t, "the boot", applyFacts(t, pool, engine)("boot", intake.DaemonBooted{}))
	resumed := lastStart(t, pool, key, role)
	if !strings.Contains(resumed, " Resume the existing phase work.") {
		t.Fatalf("the boot's start of the %s of %s = %q, want the resume of its phase", role, key, resumed)
	}
	return resumed
}

// Each step's task names the pull request of its own task, in a resume as in a start, so a worker
// resumed in a fresh session, which has no earlier task to read it from, is told the number of the
// pull request it works on, and the merger the head it may merge. Of the example plan a to e, each
// task with its own open pull request, a's #1 on the issue's own branch and e's #5 on
// legion/LEGION-208.e: the merger of e, the task that merges, resumed by the boot while a to d are
// done, is told ` Pull request: #5.` right after the lines of where its work stands, and no other
// number; resumed at the head a merge forward landed past the review that the reviewer is asked to
// read, it is told after that line to merge the head only once the reviewer's approval of it is on
// the pull request, as its start is; the reviewer of c, resumed by the boot while c stands at its
// review, is told c's ` Pull request: #3.`; and the start of c's review, at the completion of c's
// implementation, names #3 once.
//
// The fault it catches is a resumed merger told no number, which reads the pull request on
// legion/<KEY>, a's, by the prompts' fallback (`prompts/go/worker-common.md`), and merges it (its
// control: the resume task composed with no pull request, as before each step's task named its
// own, under which the resumed merger names no number), and a resumed merger not told that the head
// it stands at waits for the reviewer's approval (its control: the resume without the merge forward
// sentence, under which the resumed merger is not told it).
func TestAResumedStepNamesItsTasksPullRequest(t *testing.T) {
	done := func(at int) exampleStand { return exampleStand{state: record.TaskDone, place: atStep(at)} }
	waiting := exampleStand{state: record.TaskWaiting}
	atMerge := exampleStand{state: record.TaskRunning, place: record.TaskPlace{Kind: record.PlaceMerge}}
	namesOnce := func(t *testing.T, who, task string, number int) {
		t.Helper()
		if want := fmt.Sprintf(" Pull request: #%d.", number); !strings.Contains(task, want) || strings.Count(task, " Pull request: #") != 1 {
			t.Fatalf("the %s's task = %q, want it to name its task's pull request #%d once, and no other", who, task, number)
		}
	}
	const mergerSteps = " Task: e. Steps: 1 implementer:i, 2 tester:t then Legion's retro and merge."

	t.Run("the merger of the task that merges, resumed by the boot", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := seedExamplePlan(t, pool, phase.Merging, map[string]exampleStand{"a": done(2), "b": done(3), "c": done(2), "d": done(2), "e": atMerge})

		merger := bootResumes(t, pool, engine, root.Key, claim.RoleMerger)
		namesOnce(t, "resumed merger of e", merger, 5)
		if want := mergerSteps + " Pull request: #5. Resume the existing phase work."; !strings.HasSuffix(merger, want) {
			t.Fatalf("the resumed merger's task = %q, want it to end %q: its pull request right after where its work stands", merger, want)
		}
	})

	t.Run("the merger resumed at a merge forward the reviewer is asked to read", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := seedExamplePlan(t, pool, phase.Merging, map[string]exampleStand{"a": done(2), "b": done(3), "c": done(2), "d": done(2), "e": atMerge})
		landed := examplePR(root.Key, "e", 5)
		landed.MergeForward = &record.MergeForward{Head: "head-e-before", Base: "main", Phase: phase.Merging, Why: record.ConflictMergeNext,
			AskedAt: stepClock, Landed: landed.HeadSHA, ReviewAsked: true}
		seedPR(t, pool, landed)

		merger := bootResumes(t, pool, engine, root.Key, claim.RoleMerger)
		namesOnce(t, "resumed merger of e", merger, 5)
		if want := mergerSteps + " Pull request: #5. Head head-e is the merge of main forward that landed after the review, and the reviewer is asked to read it:" +
			" merge head head-e only once the reviewer's approval of it is on the pull request. Resume the existing phase work."; !strings.HasSuffix(merger, want) {
			t.Fatalf("the resumed merger's task = %q, want it to end %q", merger, want)
		}
	})

	t.Run("the reviewer of a task that does not merge, resumed by the boot", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := seedExamplePlan(t, pool, phase.Reviewing, map[string]exampleStand{
			"a": done(2), "b": done(3), "c": {state: record.TaskRunning, place: atStep(2)}, "d": waiting, "e": waiting,
		})

		reviewer := bootResumes(t, pool, engine, root.Key, claim.RoleReviewer)
		namesOnce(t, "resumed reviewer of c", reviewer, 3)
		if want := " Task: c. Step 2 of 2: reviewer:r. Steps: 1 implementer:i, 2 reviewer:r. Pull request: #3. Resume the existing phase work."; !strings.Contains(reviewer, want) {
			t.Fatalf("the resumed reviewer's task = %q, want it to hold %q", reviewer, want)
		}
	})

	t.Run("the start of a step names its task's pull request once", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := seedExamplePlan(t, pool, phase.Implementing, map[string]exampleStand{
			"a": done(2), "b": done(3), "c": {state: record.TaskRunning, place: atStep(1)}, "d": waiting, "e": waiting,
		})

		taskMoves(t, pool, engine, root.Key, "c", TriggerImplementationReady, "")

		namesOnce(t, "started reviewer of c", lastStart(t, pool, root.Key, claim.RoleReviewer), 3)
	})
}

// pullReads is the real store, counting the reads of the pull request of task of key, whole or as
// its row alone: the task's own row (record.Store.PullRequestOfTask, PullRequestRowOfTask), and for
// task "", an issue with no plan rows, the row of its merge (record.Store.MergePullRequest,
// MergePullRequestRow). A handler reads the row whole to judge its move, and a start with no row
// handed on reads it alone (standingPart, taskPullRequestRow), so a start that read the row again
// is counted whichever read it made. Every other read and write is the real store's.
type pullReads struct {
	record.Store
	key, task string
	reads     int
}

func (s *pullReads) PullRequestOfTask(ctx context.Context, tx pgx.Tx, issue, task string) (*record.PullRequest, error) {
	if issue == s.key && task == s.task {
		s.reads++
	}
	return s.Store.PullRequestOfTask(ctx, tx, issue, task)
}

func (s *pullReads) MergePullRequest(ctx context.Context, tx pgx.Tx, issue string) (*record.PullRequest, error) {
	if issue == s.key && s.task == "" {
		s.reads++
	}
	return s.Store.MergePullRequest(ctx, tx, issue)
}

func (s *pullReads) PullRequestRowOfTask(ctx context.Context, tx pgx.Tx, issue, task string) (*record.PullRequest, error) {
	if issue == s.key && task == s.task {
		s.reads++
	}
	return s.Store.PullRequestRowOfTask(ctx, tx, issue, task)
}

func (s *pullReads) MergePullRequestRow(ctx context.Context, tx pgx.Tx, issue string) (*record.PullRequest, error) {
	if issue == s.key && s.task == "" {
		s.reads++
	}
	return s.Store.MergePullRequestRow(ctx, tx, issue)
}

// countsPullReads makes engine count, from now on, the reads of the pull request of task of key
// (pullReads), and answers the count.
func countsPullReads(engine *Engine, key, task string) *pullReads {
	reads := &pullReads{Store: engine.store, key: key, task: task}
	engine.store = reads
	return reads
}

// A start that its handler judged on the pull request of its task reads that row once in its
// transaction: the handler reads it to judge its move (whether the move needs a pull request, the
// review round, whether GitHub merged it), and the started step's task names that row
// (Standing.pull) with no read of its own. Of the example plan, the implementer's completion of c,
// which starts c's review through the transition, reads c's row once; so does the tester's pass of
// an issue with no plan rows, which starts its review on the row of the issue's merge, the retry of
// its held implementing, which starts its implementer again (retryPhase), and the gate's opening of
// a plan of one task, whose task takes the issue's branch and its pull request #42 there
// (startReady). Each start names the row it read.
//
// The fault it catches is a start that reads the row again for its text after its handler read
// it, two reads of one row in one transaction (its control: standingPart reading the row whatever
// its caller read, under which each of the four reads it twice), and a merge forward after a move
// that reads the row the move read again (its controls: mergeForwardOnEntry reading it, under which
// the two completions, which move past implementing, read it twice; clearTaskMergeForward reading
// it, under which the gate's opening, a move into implementing, reads it twice).
func TestAStartReadsItsTasksPullRequestOnce(t *testing.T) {
	readsOnce := func(t *testing.T, who string, reads *pullReads, task string, number int) {
		t.Helper()
		if reads.reads != 1 {
			t.Fatalf("%s read the pull request of task %q of %s %d times, want once", who, reads.task, reads.key, reads.reads)
		}
		if want := fmt.Sprintf(" Pull request: #%d.", number); strings.Count(task, " Pull request: #") != 1 || !strings.Contains(task, want) {
			t.Fatalf("the task %s started = %q, want it to name the pull request #%d it read, once", who, task, number)
		}
	}

	t.Run("a completion that starts the next step of its task", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := seedExamplePlan(t, pool, phase.Implementing, map[string]exampleStand{
			"a": {state: record.TaskDone, place: atStep(2)}, "b": {state: record.TaskDone, place: atStep(3)},
			"c": {state: record.TaskRunning, place: atStep(1)}, "d": {state: record.TaskWaiting}, "e": {state: record.TaskWaiting},
		})
		reads := countsPullReads(engine, root.Key, "c")

		mustSpend(t, "the implementer's completion of c", applyFacts(t, pool, engine)("implement-c", intake.HandoffComplete{Generation: 1, Issue: root.Key,
			Role: claim.RoleImplementer, Claim: agentClaim(t, root.Key, "implementer-i"), Task: "c", Summary: "done", Commit: "head-c",
			HandoffRecord: "implement-1", HandoffVersion: 1}))

		reviewer := lastStart(t, pool, root.Key, claim.RoleReviewer)
		if !strings.Contains(reviewer, " Task: c. Step 2 of 2: reviewer:r.") {
			t.Fatalf("the reviewer's start = %q, want c's review", reviewer)
		}
		readsOnce(t, "the implementer's completion of c", reads, reviewer, 3)
	})

	t.Run("a completion of an issue with no plan rows", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := stepRoot(t, pool, phase.Testing)
		reads := countsPullReads(engine, root.Key, "")

		completes(t, pool, engine, claim.RoleTester, "test", 1)

		readsOnce(t, "the tester's pass", reads, lastStart(t, pool, root.Key, claim.RoleReviewer), 42)
	})

	t.Run("the retry of a held phase of an issue with no plan rows", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := stepRoot(t, pool, phase.Implementing)
		root.Phase, root.Hold = phase.Held, &record.Hold{From: phase.Implementing}
		seedIssue(t, pool, root)
		reads := countsPullReads(engine, root.Key, "")

		mustSpend(t, "the retry", applyFacts(t, pool, engine)("retry", intake.RetryOrEscalate{Issue: root.Key, Decision: intake.RetryDecision, Reason: "the claim failed"}))

		readsOnce(t, "the retry", reads, lastStart(t, pool, root.Key, claim.RoleImplementer), 42)
	})

	t.Run("the gate's opening of a plan of one task", func(t *testing.T) {
		pool, engine := migratedPool(t), textEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:a", "reviewer:a"}, agents: []claim.Agent{"impl-a", "rev-a"}, state: record.TaskWaiting})
		reads := countsPullReads(engine, root.Key, "a")

		firstStepOf(t, pool, engine, root.Key)

		readsOnce(t, "the gate's opening", reads, lastStart(t, pool, root.Key, claim.RoleImplementer), 42)
	})
}
