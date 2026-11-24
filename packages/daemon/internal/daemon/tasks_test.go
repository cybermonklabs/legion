// The whole-daemon tests of a plan of several tasks (tasksRig): the booted daemon over the real
// Dispatch of this package's tests, the fake runtime, and a fake GitHub whose refs, commits and
// pull requests are a real bare git repository's (tasksGitHub), so each branch the daemon makes
// for a task, each join of a task's needs and each pull request an implementer opens is one git
// can read. Each task's working copy is made for real, a jj workspace of one clone of that
// repository, when a step's task adopts it (tasksCopies). The owner of one issue gives it a plan of
// several tasks and opens its gate; the test plays each agent of each step, task by task, and reads
// where each task stands from the store. Beside them, the issues of one task that the release
// before several tasks left mid-walk, booted on that release's store (acrossIssue).
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// examplePlan is the example plan of several tasks in the form this release takes, each need the
// reference to its task's block of plan.md that %[1]s begins (givePlanText): a and b need nothing
// and run at one time, c needs b, d needs a and c, and e needs d, whose work merges. This release
// runs one agent of each role for an issue, so the steps of a role share one key in every task,
// and the two tasks that run at one time start at steps of two roles: a at its implementer, b at
// its planner.
const examplePlan = `:::plan_task{#a steps="implementer:i reviewer:r"}
Change the parser of the widget.
:::

:::plan_task{#b steps="planner:p implementer:i reviewer:r"}
Design the store of the widget, then change it.
:::

:::plan_task{#c needs="%[1]sb" steps="implementer:i reviewer:r"}
Move the readers of the store.
:::

:::plan_task{#d needs="%[1]sa %[1]sc" steps="implementer:i reviewer:r"}
Join the parser and the store.
:::

:::plan_task{#e needs="%[1]sd" steps="implementer:i tester:t"}
Check the whole change.
:::
`

// The example plan runs its tasks at one time. Once the gate opens, a and b both run, each at its
// first step, before either is done, and c, d and e wait. b's planner ends while a's implementer
// works, so b's implementer step waits for the one implementer, and starts when a's step ends. c
// waits until b is done, d until a and c are, and e until d is; each starts once they are, and the
// issue merges e's pull request. Every launch is in the issue's one pod, and the implementer and
// the reviewer each run every step of theirs in the one session of their first launch.
//
// The fault: a task that starts before the tasks it needs are done works from a base without
// their work. The control is a mutation of the release's code, run by hand: workflow's needsDone
// answering true for every task, so c starts at the gate beside b, and the check that c waits
// fails.
func TestTheExamplePlanRunsItsTasksAtOneTime(t *testing.T) {
	t.Parallel()
	r := newTasksRig(t)
	r.walkExamplePlan()
	r.onePod(claim.RoleImplementer, claim.RoleReviewer, claim.RolePlanner, claim.RoleTester)
}

// Each task of the example plan works in its own working copy, from its join. The walk is the
// example plan's (walkExamplePlan): each step's task names the working copy of its own task, which
// the runtime's adoption makes for real (tasksCopies), and each implementer commits in the
// directory its task names and pushes its task's branch. a and b, which run at one time, work in two
// directories: the issue's own, on legion/<KEY>, and the one of legion/<KEY>.b beside it. c starts
// from b's work; d starts from the join of a's and c's, so the last commit of each is an ancestor
// of the commit d's working copy started at; and e starts from d's, so the last commit of every
// other task is an ancestor of e's start. Every launch is in the issue's one pod, and the issue
// merges.
//
// The fault: a task whose working copy starts without the work of a task it needs, or two tasks
// that run at one time in one directory. Its controls are run by hand. As the approved check states
// it: examplePlan with the needs of c taken out (d then needs b too, so the plan keeps its one task
// whose work merges): c starts at the gate beside a and b, before b is done, and the check that c
// waits fails. The executor's join of a task's needs that merges its first need alone: d's start
// lacks c's last commit, e's lacks b's and c's, and the ancestor checks fail. The adoption that
// names no branch: no working copy of b is made, and the check of the directory b's implementer is
// told fails.
func TestEachTaskOfTheExamplePlanWorksInItsOwnCopyFromItsJoin(t *testing.T) {
	t.Parallel()
	r := newTasksRig(t)
	r.walkExamplePlan()
	issue, b := claim.IssueBranch(r.key), r.branchOf("b")
	if a := r.branchOf("a"); a != issue {
		t.Fatalf("task a's branch is %s, want the issue's own, %s", a, issue)
	}
	dirs := r.copies.made()
	if dirs[issue] == "" || dirs[b] == "" || dirs[issue] == dirs[b] {
		t.Fatalf("the working copies of a and b, made at their adoption, are %q and %q, want two directories", dirs[issue], dirs[b])
	}
	for _, task := range []struct {
		id    string
		needs []string
	}{{"c", []string{"b"}}, {"d", []string{"a", "c"}}, {"e", []string{"a", "b", "c", "d"}}} {
		for _, need := range task.needs {
			if !r.copies.ancestor(r.lasts[need], r.starts[task.id]) {
				t.Errorf("task %s's working copy started at %s, which does not hold %s's last commit %s", task.id, r.starts[task.id], need, r.lasts[need])
			}
		}
	}
	r.onePod(claim.RoleImplementer, claim.RoleReviewer, claim.RolePlanner, claim.RoleTester)
}

// walkExamplePlan gives the issue examplePlan and walks it to its merge, as
// TestTheExamplePlanRunsItsTasksAtOneTime says.
func (r *tasksRig) walkExamplePlan() {
	r.t.Helper()
	r.givePlanText(examplePlan)
	r.firstStep()
	r.waitAt("a", 1)
	r.waitAt("b", 1)
	r.stillWaiting("both a and b run", "c", "d", "e")

	r.planStep("b")
	r.waitAgent("b", 2)
	r.implementStep("a", "main")
	r.waitAt("b", 2)
	r.reviewStep("a")
	r.waitDone("a")
	r.stillWaiting("a is done and b is not", "c", "d", "e")

	r.implementStep("b", "main")
	r.stillWaiting("b reviews", "c", "d", "e")
	r.reviewStep("b")
	r.waitDone("b")
	r.waitAt("c", 1)
	r.stillWaiting("c runs", "d", "e")

	r.implementStep("c", r.branchOf("b"))
	r.stillWaiting("c reviews", "d", "e")
	r.reviewStep("c")
	r.waitDone("c")
	r.waitAt("d", 1)
	r.stillWaiting("d runs", "e")

	r.implementStep("d", r.branchOf("d")+claim.JoinBranchSuffix)
	r.stillWaiting("d reviews", "e")
	r.reviewStep("d")
	r.waitDone("d")
	r.waitAt("e", 1)

	r.implementStep("e", "main")
	r.testStep("e")
	r.retroAndMerge("e")
}

// agentsPlan is two tasks, x and y, that need nothing and whose first steps are of one key, k, and
// the task z that needs both, whose work merges, each need written as examplePlan's.
const agentsPlan = `:::plan_task{#x steps="implementer:k reviewer:r"}
Change the parser of the widget.
:::

:::plan_task{#y steps="implementer:k reviewer:r"}
Change the store of the widget.
:::

:::plan_task{#z needs="%[1]sx %[1]sy" steps="implementer:k reviewer:r"}
Join the parser and the store.
:::
`

// The agents of tasks with one last task: x and y start at one time, each at a step of the key k,
// whose one agent works x's step first (task id order) while y's step waits for it, given no task
// of its own; once x's step ends, y's step starts in the same session, with no launch of a second
// agent.
//
// The fault: a step of a key that starts while its agent works a step of another task gives the
// one agent two tasks at once. The control is a mutation of the release's code, run by hand:
// workflow's waitForAgent answering that no step waits, so y's step starts at the gate beside x's,
// and the check that y's step waits for its agent fails.
func TestTheAgentsOfTasksWithOneLastTask(t *testing.T) {
	t.Parallel()
	r := newTasksRig(t)
	r.givePlanText(agentsPlan)
	r.firstStep()
	r.waitAt("x", 1)
	r.waitAgent("y", 1)
	k := r.worker(claim.RoleImplementer)
	r.implementStep("x", "main")
	r.waitAt("y", 1)
	if again := r.worker(claim.RoleImplementer); again != k {
		t.Fatalf("y's step went to the claimant of session %s, want the one of x's step, %s", again.session, k.session)
	}
	r.implementStep("y", "main")
	if got := r.d.claim(k.token).Session; got != k.session {
		t.Errorf("the claim of k holds the session %q after y's step, want %q, the session of x's step", got, k.session)
	}
	r.onePod(claim.RoleImplementer)
}

// conflictPlan is two tasks, a and c, that need nothing and change one line of one file, x.txt;
// the task d that needs both, in that order; and the task e that needs d, so d does not merge and
// its pull request is based on its join branch. Each need is written as examplePlan's.
const conflictPlan = `:::plan_task{#a steps="implementer:i reviewer:r"}
Change the parser of the widget.
:::

:::plan_task{#c steps="implementer:i reviewer:r"}
Move the readers of the store.
:::

:::plan_task{#d needs="%[1]sa %[1]sc" steps="implementer:i reviewer:r"}
Join the parser and the store.
:::

:::plan_task{#e needs="%[1]sd" steps="implementer:i tester:t"}
Check the whole change.
:::
`

// A join that conflicts is repaired by the first step of the task that needs it. a and c each
// change the one line of x.txt that main holds, so GitHub refuses the merge of c into d's join
// branch, which the join makes at a's head. d's first step's task tells its agent how to repair
// that join, and the agent does what it says in d's working copy, which the adoption made
// (repairAndWork): the join branch then holds a's and c's last commits and x.txt with both lines,
// no conflict marker, and d's own work starts from that join.
//
// The fault: a task whose first step is not told to repair its join starts without the work of the
// need whose merge conflicted. The control is a mutation of the release's code, run by hand:
// workflow's joinToRepair answering nothing, so the agent, which acts on that text alone, repairs
// no join, and the check that d's join branch holds c's last commit fails.
func TestAJoinThatConflictsIsRepairedByTheFirstStep(t *testing.T) {
	t.Parallel()
	r := newTasksRig(t)
	r.gh.repo.Commit(t, "main", map[string]string{"x.txt": "line 1\n"}, "the file a and c change")
	r.givePlanText(conflictPlan)
	r.firstStep()
	r.waitAt("a", 1)
	r.waitAgent("c", 1)
	r.implementWith("a", "main", r.changeX("a"))
	r.waitAt("c", 1)
	r.reviewStep("a")
	r.waitDone("a")
	r.implementWith("c", "main", r.changeX("c"))
	r.reviewStep("c")
	r.waitDone("c")
	r.waitAt("d", 1)

	d := r.branchOf("d")
	r.implementWith("d", d+claim.JoinBranchSuffix, r.repairAndWork)
	join := r.gh.repo.Head(t, d+claim.JoinBranchSuffix)
	for _, need := range []string{"a", "c"} {
		if !r.copies.ancestor(r.lasts[need], join) {
			t.Errorf("d's join branch stands at %s, which does not hold %s's last commit %s", join, need, r.lasts[need])
		}
	}
	if x := r.copies.show(join, "x.txt"); x != resolvedX {
		t.Errorf("x.txt on d's join branch = %q, want the line of each need with no conflict marker, %q", x, resolvedX)
	}
	if !r.copies.ancestor(join, r.starts["d"]) {
		t.Errorf("d's own work started from %s, which does not hold its join branch's head %s", r.starts["d"], join)
	}
	if x := r.copies.show(r.lasts["d"], "x.txt"); x != resolvedX {
		t.Errorf("x.txt on d's branch = %q, want the repaired join's %q", x, resolvedX)
	}
}

// changeX is the work of task's implementer in conflictPlan: the line of x.txt changed to its own.
func (r *tasksRig) changeX(task string) func(told, dir, branch string) (string, string) {
	return func(_, dir, branch string) (string, string) {
		return r.copies.commit(dir, branch, task, map[string]string{"x.txt": "from " + task + "\n"})
	}
}

// repairAndWork is the work of d's implementer in conflictPlan, which acts on what its task, told,
// says of the join of its needs and on nothing else: where the task tells jj commands, it runs each
// in d's working copy dir (taskCopy.follow), repairing x.txt with the line of each need
// (resolvedX). Its own work, d.txt, is then described and pushed on d's branch (taskCopy.work). It
// answers the commit its own work started from and the one it pushed.
func (r *tasksRig) repairAndWork(told, dir, branch string) (string, string) {
	r.t.Helper()
	d := taskCopy{t: r.t, run: fixturegit.Isolated(r.t, r.copies.home), dir: dir}
	if jjCommand.MatchString(told) {
		d.follow(told, map[string]string{"x.txt": resolvedX})
	}
	start := d.jj("log", "--no-graph", "-r", "@-", "-T", "commit_id")
	d.work(branch, map[string]string{"d.txt": "the work of task d\n"}, "the work of task d")
	return start, r.gh.repo.Head(r.t, branch)
}

// tasksRig is the whole daemon with one issue at admitted whose owner gives it a plan of several
// tasks (ownerStepsRig), over the fake GitHub of a real repository (gh), with the working copy of
// each task made for real when a step's task adopts it (copies); and, by task, the pull request its
// implementer opened, the commit its working copy started at, and the last commit it pushed.
type tasksRig struct {
	*ownerStepsRig
	gh     *tasksGitHub
	copies *tasksCopies
	pulls  map[string]int
	starts map[string]string
	lasts  map[string]string
}

// newTasksRig is newTasksRigWith for one issue over the package's broker and a Dispatch of this
// checkout, which the package's own tests run.
func newTasksRig(t *testing.T) *tasksRig {
	t.Helper()
	return newTasksRigWith(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), "One change split in several tasks")
}

// newTasksRigWith sets up dispatch, a running Dispatch, with the run's project and its repository
// mapping, one issue of each of titles created with a spec and seeded at admitted with no design
// gate and no pull request (seedAdmitted), and boots the daemon of proof at it over the fake GitHub
// of a real repository, with each issue's architect registered. Nothing of an issue starts until
// its gate opens (firstStep). The steps act on the first issue until on moves them. Dispatch
// refuses an issue whose title is a near copy of an open one's, so no two titles share a word
// that counts.
func newTasksRigWith(t *testing.T, proof liveProof, dispatch *scratchDispatch, titles ...string) *tasksRig {
	t.Helper()
	// The daemon's own log, the one a run names (liveDaemonLogEnv) or one of the test's own, is
	// printed when the test fails. The test's own is handed to the proof (liveProof.logPath)
	// rather than set in the process's environment, which every test of the package shares.
	logPath := os.Getenv(liveDaemonLogEnv)
	if logPath == "" {
		logPath = filepath.Join(t.TempDir(), "daemon.log")
		proof.logPath = logPath
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		written, err := os.ReadFile(logPath)
		if err != nil {
			t.Logf("read the daemon's log %s: %v", logPath, err)
			return
		}
		t.Logf("the daemon's log:\n%s", written)
	})
	gh := newTasksGitHub(t)
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "TK" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	// The boot reads the GitHub it is given from the walk's stand-in (stepRecordsRig.boot); every
	// read of this rig's GitHub goes to gh.
	r := &tasksRig{ownerStepsRig: &ownerStepsRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch,
		github: &stepRecordsGitHub{t: t, url: gh.url}, claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)},
		specs: map[string]string{}}, gh: gh, pulls: map[string]int{}, starts: map[string]string{}, lasts: map[string]string{}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Several tasks"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for _, title := range titles {
		var spec struct {
			createdIssue
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": title,
			"spec": "The issue's change, as its owner states it.\n"}, http.StatusCreated, &spec)
		if spec.PrimaryArtifactID == "" {
			t.Fatalf("Dispatch created %s with no primary document", spec.Key)
		}
		created := spec.createdIssue
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		r.specs[created.Key] = spec.PrimaryArtifactID
		r.issues = append(r.issues, walkIssue{key: created.Key, title: title})
		r.on(created.Key)
		r.seedAdmitted(created.Rank, created.LastSeq)
	}
	r.boot()
	r.copies = newTasksCopies(r)
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	r.on(r.issues[0].key)
	return r
}

// givePlanText writes form as plan.md on the issue, each %[1]s in it the reference to a block of
// that document as Dispatch writes one, `dispatch://<KEY>/artifact/plan-md#b-`, which the task's id
// ends, and has its architect give version 1 of it, which the daemon must take.
func (r *tasksRig) givePlanText(form string) {
	r.t.Helper()
	artifact := r.document("plan.md", fmt.Sprintf(form, "dispatch://"+r.key+"/artifact/plan-md#b-"))
	if status, body := r.registerPlan(artifact, 1); status != http.StatusOK {
		r.t.Fatalf("register_plan of %s's plan = %d %s, want 200", r.key, status, body)
	}
}

// tasksRow is where one task of the issue stands, as plan_tasks holds it: its state, the kind and
// place of its act, what its step waits for, and the branch its first start gave it.
type tasksRow struct {
	state, kind string
	position    int
	wait        string
	branch      string
}

// rows is each task of the issue's generation, by id.
func (r *tasksRig) rows() map[string]tasksRow {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select task, state, running_kind, position, step_wait, branch from plan_tasks
		where issue = $1 and generation = (select generation from issues where key = $1)`, r.key)
	if err != nil {
		r.t.Fatalf("read the tasks of %s: %v", r.key, err)
	}
	defer rows.Close()
	tasks := map[string]tasksRow{}
	for rows.Next() {
		var task string
		var row tasksRow
		if err := rows.Scan(&task, &row.state, &row.kind, &row.position, &row.wait, &row.branch); err != nil {
			r.t.Fatal(err)
		}
		tasks[task] = row
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return tasks
}

// waitTask waits for task to stand as holds says, what in the failure's words.
func (r *tasksRig) waitTask(task, what string, holds func(tasksRow) bool) {
	r.t.Helper()
	var last tasksRow
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		last = r.rows()[task]
		return holds(last)
	}) {
		r.t.Fatalf("task %s of %s stands %+v, want %s", task, r.key, last, what)
	}
}

// waitAt waits for task to run its step at place, started: no wait for its join or its agent.
func (r *tasksRig) waitAt(task string, place int) {
	r.t.Helper()
	r.waitTask(task, fmt.Sprintf("running its step %d, started", place), func(row tasksRow) bool {
		return row.state == string(record.TaskRunning) && row.kind == "step" && row.position == place && row.wait == string(record.StepWaitNone)
	})
}

// waitAgent waits for task's step at place to wait for its agent, which works a step of another task.
func (r *tasksRig) waitAgent(task string, place int) {
	r.t.Helper()
	r.waitTask(task, fmt.Sprintf("running at its step %d, which waits for its agent", place), func(row tasksRow) bool {
		return row.state == string(record.TaskRunning) && row.kind == "step" && row.position == place && row.wait == string(record.StepWaitAgent)
	})
}

// waitDone waits for task to be done.
func (r *tasksRig) waitDone(task string) {
	r.t.Helper()
	r.waitTask(task, "done", func(row tasksRow) bool { return row.state == string(record.TaskDone) })
}

// stillWaiting fails the test unless each of tasks still waits for the tasks it needs, when the
// walk stands where when says.
func (r *tasksRig) stillWaiting(when string, tasks ...string) {
	r.t.Helper()
	rows := r.rows()
	for _, task := range tasks {
		if row := rows[task]; row.state != string(record.TaskWaiting) {
			r.t.Fatalf("task %s of %s stands %+v when %s, want waiting for the tasks it needs", task, r.key, row, when)
		}
	}
}

// branchOf is the branch of task, which its first start gave it (record.PlanTask.BranchOf).
func (r *tasksRig) branchOf(task string) string {
	r.t.Helper()
	row, ok := r.rows()[task]
	if !ok {
		r.t.Fatalf("the plan of %s holds no task %s", r.key, task)
	}
	if row.branch == "" {
		return claim.IssueBranch(r.key)
	}
	return row.branch
}

// takeFor is role's claimant taking its next task, which must be of task: it names task's working
// copy, its branch.
func (r *tasksRig) takeFor(role claim.Role, task string) *stepClaimant {
	r.t.Helper()
	c, _, _ := r.takeIn(role, task)
	return c
}

// workingCopyLine is the working copy line of a task of a plan of several tasks (workflow's
// WorkingCopy): its branch, then its directory, which holds no space.
var workingCopyLine = regexp.MustCompile(` Working copy: branch (\S+), directory (\S+)\.(\s|$)`)

// takeIn is takeFor that answers the directory of the working copy the task names too, and the
// task's whole text.
func (r *tasksRig) takeIn(role claim.Role, task string) (*stepClaimant, string, string) {
	r.t.Helper()
	c := r.worker(role)
	prompt := c.take()
	line := workingCopyLine.FindStringSubmatch(prompt.Message)
	if branch := r.branchOf(task); line == nil || line[1] != branch {
		r.t.Fatalf("the %s's task = %q, want task %s's, naming its working copy on %s and its directory", role, prompt.Message, task, branch)
	}
	return c, line[2], prompt.Message
}

// done ends c's step: its handoff record of word (none for word ""), its completion req, which the
// daemon must take, and the end of its turn.
func (r *tasksRig) done(c *stepClaimant, word string, req api.HandoffCompleteRequest) {
	r.t.Helper()
	if word != "" {
		req.HandoffID, req.HandoffVersion = r.writeHandoff(c.session, word)
	}
	if status, body := r.complete(c, req); status != http.StatusOK {
		r.t.Fatalf("the %s's completion = %d %s, want 200", c.role, status, body)
	}
	c.endTurn()
}

// planStep is the planner's step of task, at its place 1.
func (r *tasksRig) planStep(task string) {
	r.t.Helper()
	r.done(r.takeFor(claim.RolePlanner, task), "plan", api.HandoffCompleteRequest{Summary: "planned " + task})
}

// implementStep is the implementer's step of task: in the working copy its task names, which the
// adoption of that task made (tasksCopies), it commits the task's work, a file of its own, and
// pushes the task's branch, opens the task's pull request against base and completes at that
// commit; the task then stands at its next step. The commit the working copy started at, and the
// one it pushed, are the task's start and last.
func (r *tasksRig) implementStep(task, base string) {
	r.t.Helper()
	r.implementWith(task, base, func(_, dir, branch string) (string, string) {
		return r.copies.commit(dir, branch, task, map[string]string{task + ".txt": "the work of task " + task + "\n"})
	})
}

// implementWith is implementStep whose work is work: what the implementer does in the working copy
// dir of branch, as the text of its task, told, says, answering the commit its work started at and
// the one it pushed on branch.
func (r *tasksRig) implementWith(task, base string, work func(told, dir, branch string) (start, last string)) {
	r.t.Helper()
	c, dir, told := r.takeIn(claim.RoleImplementer, task)
	branch := r.branchOf(task)
	if made := r.copies.made()[branch]; made != dir {
		r.t.Fatalf("task %s's implementer is told the working copy %s, want %s, the one its adoption made", task, dir, made)
	}
	at := r.rows()[task].position
	r.starts[task], r.lasts[task] = work(told, dir, branch)
	r.pulls[task] = r.gh.open(branch, base)
	r.done(c, "implement", api.HandoffCompleteRequest{Summary: "implemented " + task, Commit: r.lasts[task]})
	r.waitTask(task, fmt.Sprintf("past its step %d", at), func(row tasksRow) bool { return row.position != at || row.state != string(record.TaskRunning) })
}

// reviewStep is the reviewer's step of task: it takes its task, and a person approves the task's
// pull request at its head.
func (r *tasksRig) reviewStep(task string) {
	r.t.Helper()
	c := r.takeFor(claim.RoleReviewer, task)
	c.endTurn()
	number := r.pulls[task]
	head := r.gh.repo.Head(r.t, r.branchOf(task))
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": "APPROVED",
		"commit_id": head, "head_sha": head, "author": stepRecordsPerson, "body": "task " + task,
		"review_id": strconv.Itoa(number*10 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.publish(fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number), "github",
		fmt.Sprintf("review-%d-%d", number, r.reviews), string(payload))
}

// testStep is the tester's step of task, which passes.
func (r *tasksRig) testStep(task string) {
	r.t.Helper()
	r.done(r.takeFor(claim.RoleTester, task), "test", testCompletion)
}

// retroAndMerge is the implementer's retro of task, the task whose work merges, at its branch's
// head, then GitHub's merge of its pull request once the merger is launched; the issue is then
// done.
func (r *tasksRig) retroAndMerge(task string) {
	r.t.Helper()
	c := r.takeFor(claim.RoleImplementer, task)
	branch := r.branchOf(task)
	head := r.gh.repo.Head(r.t, branch)
	req := r.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."})
	req.Commit = head
	r.done(c, "", req)
	r.waitPhase(phase.Merging)
	r.awaitLaunch(r.token(claim.RoleMerger))
	number := r.pulls[task]
	r.gh.merged(number)
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(number), "merged": "true",
		"merge_commit_sha": "merge-" + head, "head_sha": head, "base_ref": "main", "head_ref": branch, "head_repo": liveRepo,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.publish(fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), number), "github", fmt.Sprintf("pr-%d-merged", number), string(payload))
	r.waitPhase(phase.Done)
}

// onePod fails the test unless every launch the runtime was asked for is of the issue, in its one
// pod, and each of roles was launched once: every step of its agent ran in the session of that
// launch.
func (r *tasksRig) onePod(roles ...claim.Role) {
	r.t.Helper()
	launches := map[claim.Token]int{}
	for _, call := range r.rt.Calls() {
		if call.Method != "Spawn" && call.Method != "Resume" {
			continue
		}
		if call.Spec.Issue != r.key {
			r.t.Errorf("the runtime was asked to launch %s for %s, want every launch in the pod of %s", call.Spec.Claim, call.Spec.Issue, r.key)
		}
		launches[call.Spec.Claim]++
	}
	for _, role := range roles {
		if n := launches[r.token(role)]; n != 1 {
			r.t.Errorf("the %s of %s was launched %d times, want once: each of its steps in the session of its one launch", role, r.key, n)
		}
	}
}

// tasksCopies makes, for real, the working copy each step's task names when the runtime adopts it
// for that task's branch (fake.Runtime.OnAdopt), as a pod's shim makes it: the issue's own once
// (workspace.Provision), and each task's beside it (workspace.ProvisionTask), under the daemon's
// state directory, so each is the directory the task text names (engineWorkspaceDir). They are
// workspaces of one shared clone of the fake GitHub's repository, which its provisioning reaches by
// the repository's path in place of GitHub's URL (tasksCopyRunner). An agent's commit in one, and
// its push of its task's branch to that repository, are plain git under the copies' own home
// (commit).
type tasksCopies struct {
	t *testing.T
	// keys is each issue of the rig, whose working copies are made.
	keys     []string
	stateDir string
	origin   string
	home     string
	run      workspace.Runner
	mu       sync.Mutex
	// dirs is the working copy made for each branch, by branch.
	dirs map[string]string
}

// newTasksCopies makes the working copies of r's issues at each adoption from now on. The hook is
// set on the booted runtime before any step starts; the fake's own lock, taken by the read of its
// calls after the write, orders the write before the adoption's read under that lock.
func newTasksCopies(r *tasksRig) *tasksCopies {
	t := r.t
	t.Helper()
	home := t.TempDir()
	repo := ghrepo.MustParse(liveRepo)
	c := &tasksCopies{t: t, stateDir: r.cfg.StateDir, origin: r.gh.repo.Dir(), home: home, dirs: map[string]string{},
		run: tasksCopyRunner{runner: workspace.NewRunner(workspace.CommandTimeout, tasksCopyTools(t)), github: workspace.GitHubURL(repo),
			origin: r.gh.repo.Dir(), env: append(fixtureenv.Home(home), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")}}
	for _, issue := range r.issues {
		c.keys = append(c.keys, issue.key)
	}
	r.rt.OnAdopt = func(loc runtime.Locator, _ runtime.GitIdentity, branch string) error {
		return c.adopt(loc.Claim, branch)
	}
	_ = r.rt.Calls()
	return c
}

// adopt makes the working copy of branch where none is made yet, for the claim token that adopts
// it: its issue's own first, which the working copy of each task's branch is a workspace beside.
// The issue is the branch's, or, for an adoption that names no branch, the one of the rig's issues
// the claim's token names (claim.NewToken: the issue's key lower-cased, then its role).
func (c *tasksCopies) adopt(token claim.Token, branch string) error {
	key, _, ok := claim.BranchTask(branch)
	for _, issue := range c.keys {
		if !ok && strings.Contains(string(token), "-"+strings.ToLower(issue)+"-") {
			key = issue
		}
	}
	if key == "" {
		c.t.Errorf("the adoption of %s by %s names no issue of the rig", branch, token)
		return fmt.Errorf("no issue of the rig adopts %q", branch)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := context.Background()
	log := func(line string) { c.t.Logf("provisioning logged: %s", line) }
	issue := claim.IssueBranch(key)
	if c.dirs[issue] == "" {
		working, err := workspace.Provision(ctx, c.run, workspace.Request{StateDir: c.stateDir, Repo: ghrepo.MustParse(liveRepo), Issue: key,
			DefaultBranch: "main", CredentialHelper: workspace.GitHubCredentialHelper, Log: log,
			Source: workspace.FromGitHub(func() (string, error) { return "test-installation-token", nil }, c.stateDir)})
		if err != nil {
			c.t.Errorf("provision the working copy of %s at its adoption for %s: %v", key, branch, err)
			return err
		}
		c.dirs[issue] = working.Dir
	}
	if branch == "" || c.dirs[branch] != "" {
		return nil
	}
	working, err := workspace.ProvisionTask(ctx, c.run, c.dirs[issue], branch, log)
	if err != nil {
		c.t.Errorf("provision the working copy of %s at its adoption: %v", branch, err)
		return err
	}
	c.dirs[branch] = working.Dir
	return nil
}

// made is the working copy each adoption made, by branch.
func (c *tasksCopies) made() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.dirs)
}

// commit is an agent's work on task in the working copy dir: files (path to content), committed on
// the commit the working copy stands at and pushed as branch to the repository. Nothing else is
// staged: the working copy is a checkout of its branch. It answers the commit the working copy
// started at and the one it pushed.
func (c *tasksCopies) commit(dir, branch, task string, files map[string]string) (start, last string) {
	c.t.Helper()
	git := fixturegit.Isolated(c.t, c.home)
	start = git.Run(dir, "git", "rev-parse", "HEAD")
	paths := slices.Sorted(maps.Keys(files))
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(files[path]), 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
	git.Run(dir, append([]string{"git", "add", "--"}, paths...)...)
	if staged := git.Run(dir, "git", "diff", "--cached", "--name-only"); staged != strings.Join(paths, "\n") {
		c.t.Fatalf("the working copy %s of task %s stages %q, want its own files alone, %q", dir, task, staged, paths)
	}
	git.Run(dir, "git", "commit", "--quiet", "-m", "the work of task "+task)
	last = git.Run(dir, "git", "rev-parse", "HEAD")
	git.Run(dir, "git", "push", "--quiet", c.origin, "HEAD:refs/heads/"+branch)
	return start, last
}

// show is path as commit holds it in the repository.
func (c *tasksCopies) show(commit, path string) string {
	c.t.Helper()
	return fixturegit.Isolated(c.t, c.home).Raw(c.origin, "git", "show", commit+":"+path)
}

// ancestor says whether commit is an ancestor of, or is, of in the repository.
func (c *tasksCopies) ancestor(commit, of string) bool {
	c.t.Helper()
	if commit == "" || of == "" {
		c.t.Fatalf("ask whether %q is an ancestor of %q: a task has no such commit", commit, of)
	}
	check := exec.Command("git", "-C", c.origin, "merge-base", "--is-ancestor", commit, of)
	check.Env = fixtureenv.Isolated(c.home)
	output, err := check.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false
	}
	c.t.Fatalf("git merge-base --is-ancestor %s %s: %v\n%s", commit, of, err, output)
	return false
}

// tasksCopyRunner runs provisioning's commands through the production runner with the real git
// and jj, under the copies' own home, with GitHub's URL of the repository replaced by the path of
// the fake GitHub's repository, so the shared clone's origin is that repository and nothing reaches
// a network.
type tasksCopyRunner struct {
	runner         workspace.Runner
	github, origin string
	env            []string
}

func (r tasksCopyRunner) Timeout() time.Duration { return r.runner.Timeout() }

func (r tasksCopyRunner) Run(ctx context.Context, command workspace.Command) (workspace.Result, error) {
	command.Argv = slices.Clone(command.Argv)
	for i, arg := range command.Argv {
		if arg == r.github {
			command.Argv[i] = r.origin
		}
	}
	command.Env = append(slices.Clone(r.env), command.Env...)
	return r.runner.Run(ctx, command)
}

// tasksCopyTools are the git and jj provisioning starts, each a wrapper that adds git's file
// transport to whatever allow-list the runner set, since provisioning reaches a remote over https
// alone and the repository here is a path.
func tasksCopyTools(t *testing.T) map[string]string {
	t.Helper()
	tools := map[string]string{}
	dir := t.TempDir()
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("the working copies of the tasks are made with a real %s: %v", tool, err)
		}
		wrapper := filepath.Join(dir, tool)
		script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
		writeExecutable(t, wrapper, script, 0o700)
		tools[tool] = wrapper
	}
	return tools
}

// writeExecutable writes body to path, a file of mode that a child of this process then runs.
// syscall.ForkLock is held for reading while the file is open for writing, as leakguard's
// writeCopyFile holds it: a child another test forks in that span would hold the write descriptor
// until its own exec closed it, and the kernel refuses to run a file open for writing (ETXTBSY).
func writeExecutable(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		t.Fatalf("write the executable %s: %v", path, err)
	}
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		t.Fatalf("write the executable %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("write the executable %s: %v", path, err)
	}
}

// tasksGitHub is GitHub's REST API over one real repository, liveRepo (githubtest.GitRepository):
// its branches, the joins of its branches and its pull requests are the repository's, each pull
// request clean until GitHub merges it (merged), the person of the reviews has write access, and a
// GraphQL root knows no pull request. Every other route is a 404, so nothing reaches github.com.
// Each pull request's number is the repository's plus offset, a number of the run's own: the tests
// of the package share one database, where the daemon finds a pull request by its repository and
// number, so two tests that each numbered theirs from 1 would read each other's.
type tasksGitHub struct {
	t      *testing.T
	repo   *githubtest.GitRepo
	url    string
	offset int
	mu     sync.Mutex
	// merges is each pull request GitHub merged, by number.
	merges map[int]bool
}

func newTasksGitHub(t *testing.T) *tasksGitHub {
	t.Helper()
	owner, name, _ := strings.Cut(liveRepo, "/")
	offset, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	g := &tasksGitHub{t: t, repo: githubtest.GitRepository(t, owner, name, "main"), offset: 1000 + 10*int(offset), merges: map[int]bool{}}
	prefix := "/repos/" + liveRepo
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, ofRepository := strings.CutPrefix(r.URL.Path, prefix)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && ofRepository && rest == "/collaborators/"+stepRecordsPerson+"/permission":
			_, _ = io.WriteString(w, `{"permission":"write","user":{"login":"`+stepRecordsPerson+`"}}`)
		case r.Method == http.MethodGet && ofRepository && strings.HasPrefix(rest, "/pulls/") && strings.HasSuffix(rest, "/reviews"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && ofRepository && (rest == "/pulls" || strings.HasPrefix(rest, "/pulls/")):
			g.readPulls(w, r)
		case r.Method == http.MethodPost && ofRepository && rest == "/pulls":
			g.openPull(w, r)
		case g.repo.Handler(w, r):
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
	return g
}

// numbered is pull as the repository answers it, with its number and its page the run's own
// (offset).
func (g *tasksGitHub) numbered(pull map[string]any) map[string]any {
	number, _ := pull["number"].(float64)
	pull["number"] = int(number) + g.offset
	pull["html_url"] = fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, int(number)+g.offset)
	return pull
}

// openPull answers an open of a pull request as the repository answers it, numbered.
func (g *tasksGitHub) openPull(w http.ResponseWriter, r *http.Request) {
	recorded := httptest.NewRecorder()
	g.repo.Handler(recorded, r)
	if recorded.Code != http.StatusCreated {
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		return
	}
	var pull map[string]any
	if err := json.Unmarshal(recorded.Body.Bytes(), &pull); err != nil {
		g.t.Errorf("decode the repository's opened pull request: %v", err)
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(g.numbered(pull))
}

// readPulls answers a read of one pull request or of a list as the repository answers it,
// numbered, each open one mergeable and clean, and each GitHub merged closed and merged, left out
// of a list of the open ones.
func (g *tasksGitHub) readPulls(w http.ResponseWriter, r *http.Request) {
	if number, one := strings.CutPrefix(r.URL.Path, "/repos/"+liveRepo+"/pulls/"); one {
		n, err := strconv.Atoi(number)
		if err != nil || n <= g.offset {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path, r.URL.RawPath = fmt.Sprintf("/repos/%s/pulls/%d", liveRepo, n-g.offset), ""
	}
	recorded := httptest.NewRecorder()
	g.repo.Handler(recorded, r)
	if recorded.Code != http.StatusOK {
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	shape := func(pull map[string]any) map[string]any {
		pull = g.numbered(pull)
		pull["updated_at"] = "2026-11-22T00:00:00Z"
		if g.merges[pull["number"].(int)] {
			head, _ := pull["head"].(map[string]any)
			pull["state"], pull["merged"], pull["mergeable"], pull["mergeable_state"] = "closed", true, true, "unknown"
			pull["merge_commit_sha"] = fmt.Sprintf("merge-%v", head["sha"])
			return pull
		}
		pull["mergeable"], pull["mergeable_state"] = true, "clean"
		return pull
	}
	var answer any
	if strings.HasSuffix(r.URL.Path, "/pulls") {
		var listed []map[string]any
		if err := json.Unmarshal(recorded.Body.Bytes(), &listed); err != nil {
			g.t.Errorf("decode the repository's list of pull requests: %v", err)
		}
		open := r.URL.Query().Get("state") == "" || r.URL.Query().Get("state") == "open"
		kept := []map[string]any{}
		for _, pull := range listed {
			if pull = shape(pull); !open || pull["state"] == "open" {
				kept = append(kept, pull)
			}
		}
		answer = kept
	} else {
		var pull map[string]any
		if err := json.Unmarshal(recorded.Body.Bytes(), &pull); err != nil {
			g.t.Errorf("decode the repository's pull request: %v", err)
		}
		answer = shape(pull)
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// open is an agent opening the pull request of branch against base, as `gh pr create` does, which
// the repository must take; it answers its number.
func (g *tasksGitHub) open(branch, base string) int {
	g.t.Helper()
	body, err := json.Marshal(map[string]string{"title": branch, "head": branch, "base": base, "body": "The work of " + branch + "."})
	if err != nil {
		g.t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Post(g.url+"/repos/"+liveRepo+"/pulls", "application/json", bytes.NewReader(body))
	if err != nil {
		g.t.Fatalf("open the pull request of %s against %s: %v", branch, base, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusCreated {
		g.t.Fatalf("open the pull request of %s against %s = %d %s (%v), want 201", branch, base, response.StatusCode, answer, err)
	}
	var opened struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(answer, &opened); err != nil || opened.Number == 0 {
		g.t.Fatalf("decode the opened pull request %s: %v", answer, err)
	}
	return opened.Number
}

// merged is GitHub's merge of pull request number: every read from now on answers it merged.
func (g *tasksGitHub) merged(number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.merges[number] = true
}

// taskHeldMigration ends the name of the migration that holds a task where it stands, the first one
// of the release of several tasks. It is found by its name rather than its number, so the store of
// the release before is the same whatever number a later merge gives it.
const taskHeldMigration = "_task_held.up.sql"

// acrossIssue is one issue of one task, a, that the release before several tasks left mid-walk:
// its key and title, the role of each step of its plan, the place its task stood at, whether it is
// held there for the failure of its step's claim, and the session the release before recorded for
// each of its claims.
type acrossIssue struct {
	key, title string
	steps      []claim.Role
	place      int
	held       bool
	sessions   map[claim.Role]sliceFourSession
}

// role is the role of the step the issue's task stood at.
func (i *acrossIssue) role() claim.Role { return i.steps[i.place-1] }

// stands is the phase of the step the issue's task stood at.
func (i *acrossIssue) stands() phase.Phase {
	return map[claim.Role]phase.Phase{claim.RoleImplementer: phase.Implementing, claim.RoleTester: phase.Testing,
		claim.RoleReviewer: phase.Reviewing}[i.role()]
}

// An issue of one task goes on across the release. The store is the one the release before
// several tasks leaves: every embedded migration below taskHeldMigration, applied by hand, with
// three issues of one task, a, written in that schema's columns (seedAcross): one at its step 1 of
// `implementer reviewer`, one at its step 2 of it, and one held at testing, its step 2 of
// `implementer tester`, for its tester's failed claim. The release's migrations then run on it, and
// the whole daemon boots on it over the real Dispatch of this package's tests. Each goes on from its
// step: the step's claim, resumed in the session the release before recorded, takes the step's
// task, the held one's once its owner retries it, and each reaches done (goOnAcross); no step
// before its own starts again, each claim keeps its session, and task a is done (wentOnAcross).
//
// The fault: an issue in flight at the release that starts again from its first step redoes the
// work it did. The control is a fourth issue seeded as the one at its step 2 of `implementer
// reviewer`, whose review then requests changes: the daemon starts its step 1 again, which is the
// start the fault makes, and the same check must say so.
func TestAnIssueOfOneTaskGoesOnAcrossTheRelease(t *testing.T) {
	t.Parallel()
	r, walks, control := newAcrossRig(t)
	for _, issue := range walks {
		r.on(issue.key)
		r.goOnAcross(issue)
	}
	for _, issue := range walks {
		if err := r.wentOnAcross(issue); err != nil {
			t.Fatal(err)
		}
		if got := string(r.d.state().Issues[issue.key].Phase); got != string(phase.Done) {
			t.Fatalf("%s, left at its step %d, stands in %s after its walk, want done", issue.key, issue.place, got)
		}
	}

	// The control: the issue whose review sends it back starts again at its first step, and the
	// check names it.
	r.on(control.key)
	r.review("CHANGES_REQUESTED", phase.Implementing)
	r.awaitLaunch(r.token(claim.RoleImplementer))
	err := r.wentOnAcross(control)
	if err == nil {
		t.Fatalf("control: %s, whose review sent it back to its first step, passed the check of an issue that went on from its step 2", control.key)
	}
	if want := "started again at its first step"; !strings.Contains(err.Error(), want) {
		t.Fatalf("control: the check of %s says %q, want it to say it %s", control.key, err, want)
	}
}

// goOnAcross walks issue to done from the step the release before left it at: the held one after
// its owner's retry, its tester then passing; the one at its implementer through its pull request
// and a review that approves; the one at its reviewer through that review; then each one's retro
// and merge.
func (r *sliceFourRig) goOnAcross(issue *acrossIssue) {
	t := r.t
	t.Helper()
	switch {
	case issue.held:
		c := r.worker(claim.RoleArchitect)
		c.ownTurn()
		status, body := r.d.request(http.MethodPost, "/legion/v1/phase/retry", map[string]any{"grantId": c.grant(), "issue": issue.key, "decision": "retry"}, false)
		if status != http.StatusOK {
			t.Fatalf("the owner's retry of %s = %d %s, want 200", issue.key, status, body)
		}
		r.waitPhase(phase.Testing)
		r.work(claim.RoleTester, "test", testCompletion, phase.Retro)
	case issue.place == 1:
		r.github.open(claim.IssueBranch(r.key), r.number)
		r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Reviewing)
		r.review("APPROVED", phase.Retro)
	default:
		r.review("APPROVED", phase.Retro)
	}
	r.retro()
	r.merge()
}

// wentOnAcross is the check that issue went on from the step the release before left its task at:
// no step before that one started again (step_starts), each claim the release before recorded holds
// the same session, and its task is done. The error names the first thing that does not hold.
func (r *sliceFourRig) wentOnAcross(issue *acrossIssue) error {
	r.t.Helper()
	ctx := context.Background()
	var earlier int
	if err := r.st.Pool().QueryRow(ctx, `select count(*) from step_starts where issue = $1 and task = 'a' and kind = 'step' and place < $2`,
		issue.key, issue.place).Scan(&earlier); err != nil {
		return fmt.Errorf("read the step starts of %s: %w", issue.key, err)
	}
	if earlier != 0 {
		return fmt.Errorf("%s, left at its step %d by the release before, started again at its first step: %d starts of a step before it",
			issue.key, issue.place, earlier)
	}
	for role, kept := range issue.sessions {
		if got := r.d.claim(r.tokenOf(issue.key, role)).Session; got != kept.id {
			return fmt.Errorf("%s's %s holds the session %q, want %q, the one the release before recorded", issue.key, role, got, kept.id)
		}
	}
	var state string
	if err := r.st.Pool().QueryRow(ctx, `select state from plan_tasks where issue = $1 and generation = 1 and task = 'a'`, issue.key).Scan(&state); err != nil {
		return fmt.Errorf("read task a of %s: %w", issue.key, err)
	}
	if state != string(record.TaskDone) {
		return fmt.Errorf("task a of %s is %s after its walk, want done", issue.key, state)
	}
	return nil
}

// newAcrossRig is the store the release before several tasks leaves (storeBeforeSeveralTasks),
// seeded with the three issues of one task that go on and the control (seedAcross), migrated to
// this release's schema, with the daemon booted on it at the real Dispatch of this package's tests,
// each issue's architect resumed in the session the earlier daemon recorded, and the claim of each
// step the issues stood at but the held one's.
func newAcrossRig(t *testing.T) (*sliceFourRig, []*acrossIssue, *acrossIssue) {
	t.Helper()
	proof := newLiveProofOn(t, workflowNATS(t))
	st, dsn := storeBeforeSeveralTasks(t)
	proof.st, proof.cfg.PostgresDSN = st, dsn
	proof.cfg.Project = "AR" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 8
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &sliceFourRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: startScratchDispatchOn(t, scratchDispatchOptions{}),
		github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Across the release"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	implementerReviewer := []claim.Role{claim.RoleImplementer, claim.RoleReviewer}
	walks := []*acrossIssue{
		{title: "Implementer resumes its step", steps: implementerReviewer, place: 1},
		{title: "Reviewer resumes its round", steps: implementerReviewer, place: 2},
		{title: "Tester retried after hold", steps: []claim.Role{claim.RoleImplementer, claim.RoleTester}, place: 2, held: true},
	}
	control := &acrossIssue{title: "Control sent back to its first step", steps: implementerReviewer, place: 2}
	for slot, issue := range append(slices.Clone(walks), control) {
		var created createdIssue
		status := workflow.PhaseStatus(issue.stands())
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": issue.title}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": status, "labels": []string{"legion"}}, http.StatusOK, &created)
		issue.key = created.Key
		r.issues = append(r.issues, walkIssue{key: created.Key, title: issue.title, number: int(number) + 1 + slot})
		r.on(created.Key)
		if issue.place >= 2 {
			r.github.open(claim.IssueBranch(r.key), r.number)
		}
		issue.sessions = r.seedAcross(issue, slot+1, status, created.Rank, created.LastSeq)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store the release before several tasks left to this release's schema: %v", err)
	}
	r.boot()
	for _, issue := range append(slices.Clone(walks), control) {
		r.on(issue.key)
		r.resume(claim.RoleArchitect, issue.sessions[claim.RoleArchitect])
		if kept, ok := issue.sessions[issue.role()]; ok {
			r.resume(issue.role(), kept)
		}
	}
	return r, walks, control
}

// storeBeforeSeveralTasks is a database of this test's own carrying every embedded migration whose
// version is below taskHeldMigration's, applied by hand in version order (applyMigrationsByHand):
// the schema the release before several tasks leaves.
func storeBeforeSeveralTasks(t *testing.T) (*store.Store, string) {
	t.Helper()
	dsn := testdb.Database(t, "legion_several_tasks")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the store the release before several tasks leaves: %v", err)
	}
	t.Cleanup(st.Close)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	first := 0
	for _, migration := range all {
		if strings.HasSuffix(migration.Name, taskHeldMigration) {
			first = migration.Version
		}
	}
	if first == 0 {
		t.Fatalf("no embedded migration ends %s, the first one of the release of several tasks", taskHeldMigration)
	}
	var before []migrations.Migration
	for _, migration := range all {
		if migration.Version < first {
			before = append(before, migration)
		}
	}
	applyMigrationsByHand(t, st, before)
	return st, dsn
}

// seedAcross writes the walk's issue as the release before several tasks leaves issue, with plain
// SQL in that release's columns: the root handed to Legion in slot, its tree under the workflow,
// its approved design gate, its open pull request once it is past its implementer, the owner's
// plan of issue's steps at version 1 with its task a running at issue's place, the implementer's
// phases row of its done step 1 once past it, the row of the
// step's role raised at entry 1 for that step, and the claims of the architect and of that role
// (none for the held one's tester, whose claim failed), each a launch the earlier daemon began in
// the session it recorded, which the boot resumes. The held one stands held from the phase of its
// step for a failed claim, which records no reason. It answers those sessions.
func (r *sliceFourRig) seedAcross(issue *acrossIssue, slot int, status, rank string, seq int64) map[claim.Role]sliceFourSession {
	t := r.t
	t.Helper()
	ctx := context.Background()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := r.st.Pool().Exec(ctx, statement, args...); err != nil {
			t.Fatalf("write %s in the columns of the release before several tasks: %v", what, err)
		}
	}
	key, stands := r.key, issue.stands()
	issuePhase, heldFrom := string(stands), any(nil)
	if issue.held {
		issuePhase, heldFrom = string(phase.Held), string(stands)
	}
	exec("the root "+key, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq,
		handed_over, dispatch_status, repo, default_branch, github_repo, held_from)
		values ($1, $1, $2, $3, $4, 1, $5, $6, $7, true, $5, $8, 'main', $8, $9)`,
		key, r.cfg.Project, issue.title, issuePhase, status, rank, seq, liveRepo, heldFrom)
	exec("the tree of "+key, `insert into tree_lifecycles (project, tree, epoch, authority) values ($1, $2, 1, 'workflow')`, r.project, key)
	exec("the design gate of "+key, `insert into design_gates (issue, artifact_id, latest_version, approved_version, ever_opened, step)
		values ($1, $2, 1, 1, true, 'spec')`, key, "spec-"+key)
	exec("the slot of "+key, `insert into slots (issue, index, admitted_at) values ($1, $2, now() - interval '1 day')`, key, slot)
	if issue.place >= 2 {
		exec("the pull request of "+key, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
			failing, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, state, mergeability, merge_sha)
			values ($1, $2, $3, $4, $5, now() - interval '3 hours', '[]', 0, 0, '[]', 1, '', '', 'open', 'MERGEABLE', '')`,
			key, liveRepo, r.number, claim.IssueBranch(key), stepRecordsHead)
	}
	exec("the plan of "+key, `insert into issue_plans (issue, generation, version, source, artifact_id, given_by, given_at)
		values ($1, 1, 1, 'owner', $2, $3, now() - interval '5 hours')`, key, "plan-"+key, string(r.token(claim.RoleArchitect)))
	exec("the task a of "+key, `insert into plan_tasks (issue, generation, task, plan_version, block_text, state, running_kind, position)
		values ($1, 1, 'a', 1, 'Make the change the issue asks for.', 'running', 'step', $2)`, key, issue.place)
	types := map[claim.Role]string{claim.RoleImplementer: "implementation", claim.RoleTester: "testing", claim.RoleReviewer: "review"}
	for at, role := range issue.steps {
		exec("step "+strconv.Itoa(at+1)+" of task a of "+key, `insert into plan_steps (issue, generation, task, position, role, type)
			values ($1, 1, 'a', $2, $3, $4)`, key, at+1, string(role), types[role])
	}
	if issue.place >= 2 {
		exec("the implementer's row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, pass, verdict, summary,
			last_handoff, completed_at, step_task, step_place) values ($1, 'implementer', $2, $3, 0, 0, '', 'implemented', $4,
			now() - interval '2 hours', 'a', 1)`, key, string(r.token(claim.RoleImplementer)), stepRecordsHead, "implement-record-"+key)
	}
	// The step's entry carries the resume task the earlier daemon stored for it, in the shape the
	// workflow composes a resume (workflow's Engine.ResumeTask), as seedSliceFourIssue writes it.
	resume := fmt.Sprintf("Continue %s. Issue: %s. Phase: %s. Working copy: branch %s. Resume the existing phase work.",
		issue.title, key, stands, claim.IssueBranch(key))
	exec("the "+string(issue.role())+"'s row of "+key, `insert into phases (issue, role, claim, handoff_commit, rounds, verdict, summary,
		last_handoff, entry, entry_task, entry_raised_at, step_task, step_place) values ($1, $2, $3, '', 0, '', '', '', 1, $4,
		now() - interval '1 hour', 'a', $5)`, key, string(issue.role()), string(r.token(issue.role())), resume, issue.place)
	claims := []claim.Role{claim.RoleArchitect}
	if !issue.held {
		claims = append(claims, issue.role())
	}
	sessions := map[claim.Role]sliceFourSession{}
	for _, role := range claims {
		token := r.token(role)
		kept := sliceFourSession{id: "ses_" + string(role) + "_" + strings.ToLower(key), file: filepath.Join(r.cfg.StateDir, key+"-"+string(role)+".jsonl")}
		sessions[role] = kept
		exec("the "+string(role)+"'s claim on "+key, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, boot_token_hash, serving_generation,
			tree_epoch, plugin_contract) values ($1, $2, $3, $3, $4, 1, $5, $6, 'launching', 0, 0, 0, 0, $7, 1, 1, $8)`,
			string(token), r.project, key, string(role), kept.id, kept.file, supervise.HashBootToken("interrupted-"+string(token)),
			api.OwnerPlanContract)
		writePrompt(t, r.cfg, token)
	}
	return sessions
}
