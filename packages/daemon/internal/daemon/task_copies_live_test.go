//go:build e2e

// The live proof's driver of the working copies of the tasks of one issue: the owner of an issue
// gives Legion a plan of several tasks, and the whole daemon gives each task a working copy of its
// own. Two tasks run at one time, each agent in its turn in a directory of its own; a task that
// needs others starts from the join of their work; the first step of a task whose needs conflict
// is told to repair that join on its join branch; and every launch and every adoption of a working
// copy of an issue is in that issue's one pod. scripts/e2e/task-copies.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The rig is the package's rig of a plan of several tasks (tasksRig, tasks_test.go), over two
// issues of one project on the real Dispatch this checkout builds (startHandoffDispatch), with
// dispatch_url at it: register_plan reads each plan.md from Dispatch with the daemon's own client.
// GitHub is the rig's fake API over a real bare repository (tasksGitHub), so each task's branch, the
// join of its needs and its pull request are ones git can read. Each adoption of a working copy
// that the daemon asks of the fake runtime makes that working copy for real, as the shim of an
// issue's pod makes it (the rig's tasksCopies), in the directory each step's task names. Each agent
// works where its task says: an implementer commits in its working copy and pushes its task's
// branch, and the agent of a first step that is told to repair a join runs the jj commands its task
// tells, in its working copy (the rig's repairAndWork). Each review is a person's on the run's
// broker, and the merge is GitHub's envelope there. The walk takes each task the daemon gives,
// whichever agent it goes to, and plays it as its text says (playStep), so it never asks the
// daemon for a step in an order of its own.
//
// This release runs one agent of each role for an issue: the steps of one role share one key in
// every task, and the two tasks that run at one time start at steps of two roles.
// TASK_COPIES_CONTROL names one control, a change of the driver's input that one check must fail
// on (taskCopiesControls); a run with a control never passes.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// taskCopiesControlEnv names the control a run makes; unset or empty, it makes none.
const taskCopiesControlEnv = "TASK_COPIES_CONTROL"

// taskCopiesControls is each control, by its value of taskCopiesControlEnv, and the check it must
// fail.
var taskCopiesControls = map[string]string{
	// the example issue's plan rows deleted once its plan is given: two-tasks-two-copies reads no
	// task and ends UNKNOWN, never ok.
	"rows-deleted": "two-tasks-two-copies",
	// every adoption reaches the rig's shim naming no branch, so it makes the issue's own working
	// copy alone, as a shim did before each task had a working copy of its own: b's step is told a
	// directory that no adoption made.
	"adoption-drops-branch": "two-tasks-two-copies",
	// c's needs taken out, and e needs b and d: d starts without b's work. The plan's own control,
	// c's needs taken out alone, leaves b needed by no task, a second task whose work would merge,
	// which this release refuses.
	"no-needs": "d-starts-from-the-join",
	// the commit d's working copy started at forgotten before the check reads it:
	// d-starts-from-the-join reads no start and ends UNKNOWN, never ok.
	"starts-unrecorded": "d-starts-from-the-join",
	// the agent of d's first step acts on a task that tells it no repair, and does its own work
	// alone: the join branch never takes c's work.
	"no-repair": "join-conflict-repaired",
	// the last commits of the conflict issue's a and c forgotten before the check reads them:
	// join-conflict-repaired reads no need's commit and ends UNKNOWN, never ok.
	"lasts-unrecorded": "join-conflict-repaired",
	// the runtime reports the implementer of the second issue gone, and the daemon launches it
	// again: a second launch of one agent of that issue.
	"implementer-gone": "one-pod",
	// the check reads no call of the runtime: one-pod reads no launch and ends UNKNOWN, never ok.
	"calls-unread": "one-pod",
}

// taskCopiesTitles is the title of each issue of the run: the example plan's and the conflict's.
var taskCopiesTitles = []string{"One change split in several tasks", "Overlapping edits mended on a join"}

// taskCopiesLive is one run of the proof: the rig over the run's two issues, the control the run
// makes, and why the running check read nothing; and, by issue and task (copiesKey), the commit the
// task's working copy started at, the last commit its implementer pushed, and its pull request.
// The rig's own maps of these are by task alone, and the run's two issues share task ids.
type taskCopiesLive struct {
	*tasksRig
	control    string
	unknown    string
	copyStarts map[string]string
	copyLasts  map[string]string
	copyPulls  map[string]int
}

// TestTaskCopiesLive drives the proof's checks against one booted daemon and one Dispatch, over two
// issues of one project: the first walks the example plan to its merge, two of its tasks at one
// time and each task in its own working copy, and the second is given a plan whose needs conflict,
// walked until the first step of the task that needs them has repaired their join.
func TestTaskCopiesLive(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newTaskCopiesLive).
	l := newTaskCopiesLive(t)
	example, conflict := l.issues[0].key, l.issues[1].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s and %s; daemon at dispatch_url %s, state directory %s",
		l.dispatch.base, l.cfg.Project, liveRepo, example, conflict, l.cfg.DispatchURL, l.cfg.StateDir)
	if l.control != "" {
		l.note("control %s=%s: the check %s must fail", taskCopiesControlEnv, l.control, taskCopiesControls[l.control])
	}
	l.check("two-tasks-two-copies", func() {
		l.on(example)
		l.twoTasksTwoCopies()
	})
	l.check("d-starts-from-the-join", func() {
		l.on(example)
		l.dStartsFromTheJoin()
	})
	l.check("join-conflict-repaired", func() {
		l.on(conflict)
		l.joinConflictRepaired()
	})
	l.check("one-pod", l.onePodEach)
}

// newTaskCopiesLive is the run around the checks: the broker the script names (or a skip naming
// the script), the control it names, the checkout its script runs from and bun on PATH
// (liveCLICheckout), and the rig over the run's issues on a Dispatch this checkout builds, whose
// shim makes each working copy the daemon's adoptions name (tasksCopies).
func newTaskCopiesLive(t *testing.T) *taskCopiesLive {
	t.Helper()
	fixtureenv.Live(t)
	proof := newLiveProof(t, "scripts/e2e/task-copies.sh")
	control := os.Getenv(taskCopiesControlEnv)
	if _, known := taskCopiesControls[control]; control != "" && !known {
		t.Fatalf("%s=%q names no control; the controls are %v", taskCopiesControlEnv, control, slices.Sorted(maps.Keys(taskCopiesControls)))
	}
	root := liveCLICheckout(t)
	rig := newTasksRigWith(t, proof, startHandoffDispatch(t, root).scratchDispatch, taskCopiesTitles...)
	if control == "adoption-drops-branch" {
		// The hook is set before any step starts, as tasksCopies sets its own; the read of the
		// fake's calls after the write orders it before an adoption's read under the fake's lock.
		shim := rig.rt.OnAdopt
		rig.rt.OnAdopt = func(loc runtime.Locator, id runtime.GitIdentity, _ string) error { return shim(loc, id, "") }
		_ = rig.rt.Calls()
	}
	return &taskCopiesLive{tasksRig: rig, control: control, copyStarts: map[string]string{}, copyLasts: map[string]string{}, copyPulls: map[string]int{}}
}

// check runs one of the proof's checks between its `== <name>` line and its one line of outcome:
// `ok <name> (<s>s)`; `UNKNOWN <name> (<s>s): <why>` when it read nothing to judge; or `FAIL
// <name> (<s>s)` when it failed the test. Either of the last two ends the run. The check that the
// run's control names must fail: one that passes fails the run, naming the control.
func (l *taskCopiesLive) check(name string, body func()) {
	l.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	defer func() {
		switch {
		case !l.t.Failed():
		case l.unknown != "":
			fmt.Printf("UNKNOWN %s (%ds): %s\n", name, int(time.Since(started).Seconds()), l.unknown)
		default:
			fmt.Printf("FAIL %s (%ds)\n", name, int(time.Since(started).Seconds()))
		}
	}()
	body()
	if l.control != "" && taskCopiesControls[l.control] == name {
		l.t.Fatalf("control %s=%s: the check %s passed, and the control must make it fail", taskCopiesControlEnv, l.control, name)
	}
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// nothingRead ends the running check as UNKNOWN: it read nothing to judge, as format says.
func (l *taskCopiesLive) nothingRead(format string, args ...any) {
	l.t.Helper()
	l.unknown = fmt.Sprintf(format, args...)
	l.t.Fatal("UNKNOWN: " + l.unknown)
}

// controlled is form with each old text of pairs (old, new, old, new, ...) replaced by its new,
// when the run's control is control, and form as it is otherwise. Each old text must stand in form
// exactly once, so a form that changed fails the run rather than running the control on the plan
// it was meant to change.
func (l *taskCopiesLive) controlled(control, form string, pairs ...string) string {
	l.t.Helper()
	if l.control != control {
		return form
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		if n := strings.Count(form, pairs[i]); n != 1 {
			l.t.Fatalf("control %s: the plan holds %q %d times, want once:\n%s", control, pairs[i], n, form)
		}
		form = strings.Replace(form, pairs[i], pairs[i+1], 1)
	}
	l.note("control %s: the plan given is", control)
	for _, line := range strings.Split(strings.TrimSpace(form), "\n") {
		l.note("  %s", line)
	}
	return form
}

// copiesKey is the key of task of the walk's issue in the run's maps by issue and task.
func (l *taskCopiesLive) copiesKey(task string) string { return l.key + "/" + task }

// copiesNow is each task of the walk's issue as plan_tasks holds it, printed with when: its state,
// where it runs and what its step waits for, and its branch. A read of no task is no observation:
// the check ends UNKNOWN.
func (l *taskCopiesLive) copiesNow(when string) map[string]tasksRow {
	l.t.Helper()
	rows := l.rows()
	if len(rows) == 0 {
		l.nothingRead("plan_tasks holds no task of %s %s", l.key, when)
	}
	var stands []string
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		row, at := rows[id], id+" "+rows[id].state
		if row.state == string(record.TaskRunning) {
			at += fmt.Sprintf(" at its %s %d", row.kind, row.position)
			if row.wait != string(record.StepWaitNone) {
				at += ", waiting for its " + row.wait
			}
		}
		if row.branch != "" {
			at += " on " + row.branch
		}
		stands = append(stands, at)
	}
	l.note("%s %s: %s", l.key, when, strings.Join(stands, "; "))
	return rows
}

// copiesStep is a task the daemon gave one agent of the walk's issue, as the agent reads it: its
// claimant, in a turn on it, and that claimant's role; its text; the plan's task it names (` Task:
// <id>.`); the branch and the directory of the working copy it names (` Working copy: branch
// <branch>, directory <dir>.`); the base its pull request is opened against (` Open the pull
// request of this task against <base>:`, the default branch where it names none); and its place
// among the task's steps (` Step <i> of <n>:`), 0 for the retro, which runs no step of the task.
type copiesStep struct {
	c           *stepClaimant
	role        claim.Role
	text        string
	task        string
	branch, dir string
	base        string
	place       int
}

// The lines of a task that copiesStep reads, beside the rig's workingCopyLine, and the markers jj
// writes in a file whose conflict is not repaired.
var (
	copiesTaskLine  = regexp.MustCompile(` Task: (\S+?)\.(\s|$)`)
	copiesStepLine  = regexp.MustCompile(` Step (\d+) of \d+: `)
	copiesBaseLine  = regexp.MustCompile(` Open the pull request of this task against (\S+): `)
	copiesConflicts = []string{"<<<<<<<", ">>>>>>>", "%%%%%%%", "+++++++"}
)

// startedStep is c's turn on prompt started, its model answering first, as take starts it, and the
// task read as its agent reads it (copiesStep).
func (l *taskCopiesLive) startedStep(c *stepClaimant, prompt shimwire.Prompt) copiesStep {
	l.t.Helper()
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	s := copiesStep{c: c, text: prompt.Message, base: "main", role: c.role}
	task, copyLine := copiesTaskLine.FindStringSubmatch(s.text), workingCopyLine.FindStringSubmatch(s.text)
	if task == nil || copyLine == nil {
		l.t.Fatalf("the %s of %s was given %q, want a task of the plan that names its working copy's branch and directory", c.role, l.key, s.text)
	}
	s.task, s.branch, s.dir = task[1], copyLine[1], copyLine[2]
	if place := copiesStepLine.FindStringSubmatch(s.text); place != nil {
		s.place, _ = strconv.Atoi(place[1])
	}
	if base := copiesBaseLine.FindStringSubmatch(s.text); base != nil {
		s.base = base[1]
	}
	return s
}

// takeCopy is role's claimant on the walk's issue taking its next task, its turn started: it waits
// for that task at most stepRecordsWait, and the failure says why it may not come, held.
func (l *taskCopiesLive) takeCopy(role claim.Role, held string) copiesStep {
	l.t.Helper()
	c := l.worker(role)
	select {
	case prompt := <-c.prompts:
		return l.startedStep(c, prompt)
	case <-time.After(stepRecordsWait):
		l.copiesNow("when its " + string(role) + " had no task")
		l.t.Fatalf("the %s of %s was given no task within %s: %s", role, l.key, stepRecordsWait, held)
		return copiesStep{}
	}
}

// launchedClaimant is role's claimant on the walk's issue once the daemon has launched its claim,
// registered and ready (worker), and nil before that launch.
func (l *taskCopiesLive) launchedClaimant(role claim.Role) *stepClaimant {
	l.t.Helper()
	token := l.token(role)
	if c, ok := l.claims[token]; ok {
		return c
	}
	for _, call := range l.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			return l.worker(role)
		}
	}
	return nil
}

// nextStep is the next task the daemon gives an agent of the walk's issue, its turn started,
// whichever of the issue's step roles it goes to; the walk waits for it at most stepRecordsWait,
// for what.
func (l *taskCopiesLive) nextStep(what string) copiesStep {
	l.t.Helper()
	roles := []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester}
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(50 * time.Millisecond) {
		for _, role := range roles {
			c := l.launchedClaimant(role)
			if c == nil {
				continue
			}
			select {
			case prompt := <-c.prompts:
				return l.startedStep(c, prompt)
			default:
			}
		}
		if time.Now().After(deadline) {
			l.copiesNow("when no agent had a task")
			l.t.Fatalf("no agent of %s was given a task within %s, waiting for %s", l.key, stepRecordsWait, what)
		}
	}
}

// walkTo plays each step the daemon gives the walk issue's agents (playStep) until it gives the
// first step of task, which it answers with that step's turn started and unplayed. With task "",
// it plays every step until the issue's pull request merges.
func (l *taskCopiesLive) walkTo(task string) copiesStep {
	l.t.Helper()
	what := "the issue's merge"
	if task != "" {
		what = "the first step of task " + task
	}
	for {
		s := l.nextStep(what)
		if task != "" && s.task == task && s.place == 1 {
			return s
		}
		if merged := l.playStep(s); merged && task == "" {
			return s
		}
	}
}

// playStep is the agent of s doing what its task says: a planner's plan; an implementer's work in
// its working copy (implementIn); a reviewer's turn, then a person's approval of the task's pull
// request (approveStep); a tester's pass; and the implementer's retro of the task whose work
// merges, then GitHub's merge (retroThenMerge), for which it answers true.
func (l *taskCopiesLive) playStep(s copiesStep) bool {
	l.t.Helper()
	switch {
	case s.role == claim.RolePlanner:
		l.done(s.c, "plan", api.HandoffCompleteRequest{Summary: "planned " + s.task})
		l.note("%s's planner planned in %s", s.task, s.dir)
	case s.role == claim.RoleImplementer && s.place == 0:
		l.retroThenMerge(s)
		return true
	case s.role == claim.RoleImplementer:
		l.implementIn(s, l.workOf(s.task))
	case s.role == claim.RoleReviewer:
		l.approveStep(s)
	case s.role == claim.RoleTester:
		l.done(s.c, "test", testCompletion)
		l.note("%s's tester passed in %s", s.task, s.dir)
	default:
		l.t.Fatalf("the walk has no play for the %s's task %q", s.role, s.text)
	}
	return false
}

// workOf is the files the implementer of task writes: on the conflict's issue, a and c each line 1
// of x.txt their own way, as the rig's changeX writes it, and every other task a file of its own.
func (l *taskCopiesLive) workOf(task string) map[string]string {
	if l.key == l.issues[1].key && (task == "a" || task == "c") {
		return map[string]string{"x.txt": "from " + task + "\n"}
	}
	return map[string]string{task + ".txt": "the work of task " + task + "\n"}
}

// inCopy fails the check unless the directory s's task names is the one the adoption of its branch
// made (tasksCopies). A run in which the runtime recorded no adoption of the issue read nothing.
func (l *taskCopiesLive) inCopy(s copiesStep) {
	l.t.Helper()
	if len(l.adoptionsOf(l.key)) == 0 {
		l.nothingRead("the runtime recorded no adoption of a working copy of %s", l.key)
	}
	made, adopted := l.copies.made()[s.branch]
	if !adopted {
		l.t.Fatalf("task %s's %s is told the working copy %s of %s, and no adoption made a working copy of %s", s.task, s.role, s.dir, s.branch, s.branch)
	}
	if made != s.dir {
		l.t.Fatalf("task %s's %s is told the working copy %s of %s, and the adoption of %s made %s", s.task, s.role, s.dir, s.branch, s.branch, made)
	}
}

// implementIn is the implementer's step s: in the working copy its task names, which the adoption
// of its branch made (inCopy), it writes files, commits them and pushes its task's branch
// (tasksCopies.commit), opens the task's pull request against the base its task names, and
// completes at that commit; the task then stands past the step. The commit the working copy started
// at and the one it pushed are the task's start and last.
func (l *taskCopiesLive) implementIn(s copiesStep, files map[string]string) {
	l.t.Helper()
	l.inCopy(s)
	l.finishImplement(s, func() (string, string) { return l.copies.commit(s.dir, s.branch, s.task, files) })
}

// finishImplement is the implementer's step s once it is in its working copy: work, which answers
// the commit its work started at and the one it pushed on its task's branch, then the task's pull
// request against the base its task names, and the completion at that commit, after which the task
// stands past the step.
func (l *taskCopiesLive) finishImplement(s copiesStep, work func() (start, last string)) {
	l.t.Helper()
	at := l.rows()[s.task].position
	key := l.copiesKey(s.task)
	l.copyStarts[key], l.copyLasts[key] = work()
	l.copyPulls[key] = l.gh.open(s.branch, s.base)
	l.done(s.c, "implement", api.HandoffCompleteRequest{Summary: "implemented " + s.task, Commit: l.copyLasts[key]})
	l.waitTask(s.task, fmt.Sprintf("past its step %d", at), func(row tasksRow) bool { return row.position != at || row.state != string(record.TaskRunning) })
	l.note("%s's implementer worked in %s from %s, pushed %s at %s and opened #%d against %s", s.task, s.dir, copiesAbbrev(l.copyStarts[key]), s.branch,
		copiesAbbrev(l.copyLasts[key]), l.copyPulls[key], s.base)
}

// approveStep is the reviewer's step s: its turn ends, and a person approves the task's pull
// request at its head on the run's broker.
func (l *taskCopiesLive) approveStep(s copiesStep) {
	l.t.Helper()
	s.c.endTurn()
	number := l.copyPulls[l.copiesKey(s.task)]
	head := l.gh.repo.Head(l.t, s.branch)
	l.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": "APPROVED",
		"commit_id": head, "head_sha": head, "author": stepRecordsPerson, "body": "task " + s.task,
		"review_id": strconv.Itoa(number*10 + l.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		l.t.Fatal(err)
	}
	l.publish(fmt.Sprintf("notifications.github.%s.pr.%d.review", l.subjectRepo(), number), "github",
		fmt.Sprintf("review-%d-%d", number, l.reviews), string(payload))
	l.note("%s's reviewer took its task in %s; a person approved #%d at %s", s.task, s.dir, number, copiesAbbrev(head))
}

// retroThenMerge is the implementer's retro s of the task whose work merges, at its branch's head,
// then GitHub's merge of its pull request once the merger is launched; the issue is then done.
func (l *taskCopiesLive) retroThenMerge(s copiesStep) {
	l.t.Helper()
	head := l.gh.repo.Head(l.t, s.branch)
	req := l.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."})
	req.Commit = head
	l.done(s.c, "", req)
	l.waitPhase(phase.Merging)
	l.awaitLaunch(l.token(claim.RoleMerger))
	number := l.copyPulls[l.copiesKey(s.task)]
	l.gh.merged(number)
	payload, err := json.Marshal(map[string]string{
		"action": "closed", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(number), "merged": "true",
		"merge_commit_sha": "merge-" + head, "head_sha": head, "base_ref": "main", "head_ref": s.branch, "head_repo": liveRepo,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		l.t.Fatal(err)
	}
	l.publish(fmt.Sprintf("notifications.github.%s.pr.%d", l.subjectRepo(), number), "github", fmt.Sprintf("pr-%d-merged", number), string(payload))
	l.waitPhase(phase.Done)
	l.note("%s's retro ran at %s, and GitHub merged #%d; %s is done", s.task, copiesAbbrev(head), number, l.key)
}

// twoTasksTwoCopies is the check two-tasks-two-copies, on the example plan (examplePlan): once the
// gate opens, a and b both run. b's planner takes its task first, in the working copy of b's
// branch, legion/<KEY>.b, which its adoption made; while it is in that turn, a's implementer is
// given its task in the issue's own working copy, legion/<KEY>, which its adoption made, a directory
// other than b's. Both claims are then in a turn at one time, each in its own working copy. b's
// planner then ends its step, and a's implementer does its work in its working copy.
func (l *taskCopiesLive) twoTasksTwoCopies() {
	l.t.Helper()
	l.givePlanText(l.controlled("no-needs", examplePlan,
		`{#c needs="%[1]sb" `, `{#c `, `{#e needs="%[1]sd" `, `{#e needs="%[1]sb %[1]sd" `))
	if l.control == "rows-deleted" {
		l.deletePlanRows()
		l.note("control rows-deleted: deleted %s's rows from plan_steps, plan_tasks and issue_plans", l.key)
	}
	if l.control == "adoption-drops-branch" {
		l.note("control adoption-drops-branch: every adoption reaches the shim naming no branch")
	}
	l.copiesNow("once its plan was given")
	l.firstStep()
	l.waitAt("a", 1)
	l.waitAt("b", 1)
	l.copiesNow("once its gate opened")

	b := l.takeCopy(claim.RolePlanner, "b's planner, at the gate")
	if b.task != "b" {
		l.t.Fatalf("the planner of %s was given task %s's step, want b's: %q", l.key, b.task, b.text)
	}
	l.inCopy(b)
	l.note("b's planner is in its turn in %s, the working copy of %s, which the adoption of %s made", b.dir, b.branch, b.branch)
	a := l.takeCopy(claim.RoleImplementer, fmt.Sprintf("b's planner was in its turn in the working copy of %s all that time, and a's task adopts another", b.branch))
	if a.task != "a" {
		l.t.Fatalf("the implementer of %s was given task %s's step, want a's: %q", l.key, a.task, a.text)
	}
	l.inCopy(a)
	planner, implementer := l.d.claim(b.c.token).State, l.d.claim(a.c.token).State
	if planner != string(supervise.StateWorking) || implementer != string(supervise.StateWorking) {
		l.t.Fatalf("the planner of %s is %s and its implementer %s, want both in a turn at one time, each in its own working copy", l.key, planner, implementer)
	}
	if want := claim.IssueBranch(l.key); a.branch != want {
		l.t.Fatalf("task a's working copy is on %s, want the issue's own branch %s", a.branch, want)
	}
	if want := claim.TaskBranch(l.key, "b"); b.branch != want {
		l.t.Fatalf("task b's working copy is on %s, want its own branch %s", b.branch, want)
	}
	if a.dir == b.dir {
		l.t.Fatalf("tasks a and b, in a turn at one time, both work in %s, want two working copies", a.dir)
	}
	l.note("a's implementer is in its turn in %s, the working copy of %s, while b's planner is in its turn in %s: two turns at one time in two directories", a.dir, a.branch, b.dir)
	for _, adoption := range l.adoptionsOf(l.key) {
		l.note("  the runtime adopted %s for %s", copiesBranchNamed(adoption.Branch), adoption.Locator.Claim)
	}
	l.done(b.c, "plan", api.HandoffCompleteRequest{Summary: "planned b"})
	l.implementIn(a, l.workOf("a"))
}

// copiesBranchNamed is how a note names the branch an adoption names: the issue's own working copy
// for none.
func copiesBranchNamed(branch string) string {
	if branch == "" {
		return "the issue's own working copy"
	}
	return "the working copy of " + branch
}

// dStartsFromTheJoin is the check d-starts-from-the-join, on the example plan given in
// two-tasks-two-copies: the walk plays every step the daemon gives until d's first step, which
// starts once a and c are done, and records the commit d's working copy started at; then until e's.
// The last commit of a, of b and of c are each an ancestor of d's start, which is the join of a's
// work and c's, which started from b's; and the last commit of each other task is an ancestor of
// e's start. The walk then goes on to e's merge.
func (l *taskCopiesLive) dStartsFromTheJoin() {
	l.t.Helper()
	d := l.walkTo("d")
	rows := l.copiesNow("when d's first step started")
	for _, need := range []string{"a", "c"} {
		if row := rows[need]; row.state != string(record.TaskDone) {
			l.t.Fatalf("task d of %s started when its need %s stands %+v, want done", l.key, need, row)
		}
	}
	l.implementIn(d, l.workOf("d"))
	l.note("d's join branch %s stands at %s", d.branch+claim.JoinBranchSuffix, copiesAbbrev(l.gh.repo.Head(l.t, d.branch+claim.JoinBranchSuffix)))
	e := l.walkTo("e")
	l.copiesNow("when e's first step started")
	l.implementIn(e, l.workOf("e"))
	if l.control == "starts-unrecorded" {
		delete(l.copyStarts, l.copiesKey("d"))
		l.note("control starts-unrecorded: forgot the commit d's working copy started at")
	}
	for _, from := range []struct {
		task  string
		holds []string
	}{{"d", []string{"a", "b", "c"}}, {"e", []string{"a", "b", "c", "d"}}} {
		start := l.copyStarts[l.copiesKey(from.task)]
		var held []string
		for _, need := range from.holds {
			last := l.copyLasts[l.copiesKey(need)]
			if start == "" || last == "" {
				l.nothingRead("task %s's working copy started at %q and task %s's last commit is %q: the walk recorded no such commit", from.task, start, need, last)
			}
			if !l.copies.ancestor(last, start) {
				l.t.Fatalf("task %s's working copy started at %s, which does not hold %s's last commit %s", from.task, start, need, last)
			}
			held = append(held, need+"'s "+copiesAbbrev(last))
		}
		l.note("%s's working copy started at %s, which holds the last commit of each task before it: %s", from.task, copiesAbbrev(start), strings.Join(held, ", "))
	}
	l.walkTo("")
	l.copiesNow("once e's pull request merged")
}

// joinConflictRepaired is the check join-conflict-repaired, on the rig's plan whose a and c each
// change line 1 of x.txt, which main holds (conflictPlan): the walk plays a's and c's steps until
// d's first step, which the plan records as joined with the conflict of c, and whose task tells it
// to repair the join of c on d's join branch. The agent does what its task says in d's working copy
// with the real jj, writing both lines into x.txt (resolvedX), and then its own work, which it
// pushes on d's branch (the rig's repairAndWork); d's step completes. The join branch then holds
// a's and c's last commits and x.txt repaired, with no conflict marker; d's own work starts from it;
// and the pull request of d against the join branch lists d.txt, d's own file, alone.
func (l *taskCopiesLive) joinConflictRepaired() {
	l.t.Helper()
	l.gh.repo.Commit(l.t, "main", map[string]string{"x.txt": "line 1\n"}, "the file a and c change")
	l.givePlanText(conflictPlan)
	l.firstStep()
	d := l.walkTo("d")
	l.copiesNow("when d's first step started")
	join := d.branch + claim.JoinBranchSuffix
	l.note("the plan records d's join with the conflicts of %v", l.joinConflictsOf("d"))
	told := fmt.Sprintf(" GitHub could not join the work of task c into the join branch %s, the base of this task's pull request: their changes conflict.", join)
	if !strings.Contains(d.text, told) {
		l.t.Fatalf("d's first step is told %q, want it told %q", d.text, told)
	}
	if d.base != join {
		l.t.Fatalf("d's first step is told to open its pull request against %s, want its join branch %s", d.base, join)
	}
	l.inCopy(d)
	followed := d.text
	if l.control == "no-repair" {
		followed = ""
		l.note("control no-repair: d's agent acts on a task that tells it no repair")
	}
	l.finishImplement(d, func() (string, string) { return l.repairAndWork(followed, d.dir, d.branch) })
	if l.control == "lasts-unrecorded" {
		delete(l.copyLasts, l.copiesKey("a"))
		delete(l.copyLasts, l.copiesKey("c"))
		l.note("control lasts-unrecorded: forgot the last commits of %s's a and c", l.key)
	}

	joined := l.gh.repo.Head(l.t, join)
	if joined == "" {
		l.t.Fatalf("GitHub has no join branch %s of d", join)
	}
	for _, need := range []string{"a", "c"} {
		last := l.copyLasts[l.copiesKey(need)]
		if last == "" {
			l.nothingRead("the walk recorded no last commit of %s's task %s", l.key, need)
		}
		if !l.copies.ancestor(last, joined) {
			l.t.Fatalf("d's join branch %s stands at %s, which does not hold %s's last commit %s", join, joined, need, last)
		}
	}
	x := l.copies.show(joined, "x.txt")
	for _, marker := range copiesConflicts {
		if strings.Contains(x, marker) {
			l.t.Fatalf("x.txt on d's join branch at %s holds the conflict marker %q: %q", joined, marker, x)
		}
	}
	if x != resolvedX {
		l.t.Fatalf("x.txt on d's join branch at %s = %q, want the repair %q", joined, x, resolvedX)
	}
	key := l.copiesKey("d")
	if !l.copies.ancestor(joined, l.copyStarts[key]) {
		l.t.Fatalf("d's own work started from %s, which does not hold its join branch's head %s", l.copyStarts[key], joined)
	}
	files := l.pullFiles(join, d.branch)
	if len(files) == 0 {
		l.nothingRead("the compare of %s against %s lists no file", d.branch, join)
	}
	if !slices.Equal(files, []string{"d.txt"}) {
		l.t.Fatalf("the pull request of d against %s lists %v, want d's own file alone [d.txt]", join, files)
	}
	l.note("%s stands at %s, which holds a's and c's last commits and x.txt %q with no conflict marker; d's own work started from it, and #%d lists %v",
		join, copiesAbbrev(joined), x, l.copyPulls[key], files)
}

// joinConflictsOf is the need of task of the walk's issue whose join GitHub refused as a conflict,
// as plan_tasks holds them (record.PlanTask.JoinConflicts).
func (l *taskCopiesLive) joinConflictsOf(task string) []string {
	l.t.Helper()
	var conflicts []string
	if err := l.st.Pool().QueryRow(context.Background(), `select join_conflicts from plan_tasks
		where issue = $1 and task = $2 and generation = (select generation from issues where key = $1)`, l.key, task).Scan(&conflicts); err != nil {
		l.t.Fatalf("read the join conflicts of task %s of %s: %v", task, l.key, err)
	}
	return conflicts
}

// pullFiles is the files of a pull request of head against base, as GitHub lists them: the files
// of the fake's compare of the two, in order.
func (l *taskCopiesLive) pullFiles(base, head string) []string {
	l.t.Helper()
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Get(l.gh.url + "/repos/" + liveRepo + "/compare/" + base + "..." + head)
	if err != nil {
		l.t.Fatalf("compare %s with %s: %v", head, base, err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusOK {
		l.t.Fatalf("compare %s with %s answered %s", head, base, answer.Status)
	}
	var comparison struct {
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	if err := json.NewDecoder(answer.Body).Decode(&comparison); err != nil {
		l.t.Fatalf("decode the compare of %s with %s: %v", head, base, err)
	}
	files := make([]string, 0, len(comparison.Files))
	for _, file := range comparison.Files {
		files = append(files, file.Filename)
	}
	slices.Sort(files)
	return files
}

// onePodEach is the check one-pod, on each issue of the run: every launch the runtime was asked for
// the issue is in the pod the Sandbox runtime gives the issue (sandbox.SandboxName of its claims,
// one name for every agent of an issue); every adoption of a working copy of the issue, its own or
// a task's, went to a claim in that pod; and each agent of a step of the issue was launched once,
// so every step it ran, in every task, ran in the session of that one launch. The conflict issue
// is walked until d's first step, so its tester never starts.
func (l *taskCopiesLive) onePodEach() {
	l.t.Helper()
	if l.control == "implementer-gone" {
		l.on(l.issues[1].key)
		l.implementerGone()
	}
	steps := map[string][]claim.Role{
		l.issues[0].key: {claim.RolePlanner, claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester},
		l.issues[1].key: {claim.RoleImplementer, claim.RoleReviewer},
	}
	calls := l.rt.Calls()
	if l.control == "calls-unread" {
		calls = nil
		l.note("control calls-unread: the check reads no call of the runtime")
	}
	for _, issue := range l.issues {
		l.on(issue.key)
		pod := sandbox.SandboxName(l.token(claim.RoleArchitect))
		launches, launched := map[claim.Token]int{}, 0
		var adoptions []copiesAdoption
		for _, call := range calls {
			switch {
			case (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Issue == l.key:
				if in := sandbox.SandboxName(call.Spec.Claim); in != pod {
					l.t.Fatalf("the runtime was asked to launch %s for %s in the pod %s, want every launch of %s in its one pod %s", call.Spec.Claim, l.key, in, l.key, pod)
				}
				launches[call.Spec.Claim]++
				launched++
			case call.Method == "AdoptWorkingCopy" && l.adoptionOf(call.Locator.Claim, call.Branch) == l.key:
				adoptions = append(adoptions, copiesAdoption{Locator: call.Locator, Branch: call.Branch})
			}
		}
		if launched == 0 || len(adoptions) == 0 {
			l.nothingRead("the runtime recorded %d launches and %d adoptions of %s", launched, len(adoptions), l.key)
		}
		copies := map[string]int{}
		for _, adoption := range adoptions {
			if in := sandbox.SandboxName(adoption.Locator.Claim); in != pod {
				l.t.Fatalf("%s was adopted by %s in the pod %s, want every adoption of %s in its one pod %s", copiesBranchNamed(adoption.Branch), adoption.Locator.Claim, in, l.key, pod)
			}
			copies[copiesBranchNamed(adoption.Branch)]++
		}
		for _, role := range steps[l.key] {
			if n := launches[l.token(role)]; n != 1 {
				l.t.Fatalf("the %s of %s was launched %d times, want once: every step of its agent, in every task, in the session of its one launch in the pod %s", role, l.key, n, pod)
			}
		}
		var made []string
		for _, named := range slices.Sorted(maps.Keys(copies)) {
			made = append(made, fmt.Sprintf("%s %d times", named, copies[named]))
		}
		l.note("%s: %d launches of %d agents, each in the pod %s; adoptions of %s, each to a claim in that pod; the agent of each of %v launched once",
			l.key, launched, len(launches), pod, strings.Join(made, ", "), steps[l.key])
	}
}

// implementerGone is the control implementer-gone: the runtime reports the process of the walk
// issue's implementer gone, and the daemon launches that claim again.
func (l *taskCopiesLive) implementerGone() {
	l.t.Helper()
	token := l.token(claim.RoleImplementer)
	held := l.d.claim(token)
	if held.Locator == nil {
		l.t.Fatalf("control implementer-gone: %s has no process to end", token)
	}
	before := len(launchSpecsOf(l.rt, token))
	l.rt.Emit(runtime.Observation{Locator: *held.Locator, Kind: runtime.Gone, Detail: "the driver ended the process"})
	testwait.WithinPolling(l.t, stepRecordsWait, 50*time.Millisecond, "the daemon's launch again of "+string(token), func() bool {
		return len(launchSpecsOf(l.rt, token)) > before
	})
	l.note("control implementer-gone: the runtime reported %s gone, and the daemon launched it again", token)
}

// adoptionOf is the issue of the run whose working copy an adoption by token of branch adopts: the
// branch's issue, or, for an adoption that names no branch, the issue whose pod token's claim is
// in; "" for none of the run's.
func (l *taskCopiesLive) adoptionOf(token claim.Token, branch string) string {
	if issue, _, named := claim.BranchTask(branch); named {
		return issue
	}
	for _, issue := range l.issues {
		architect, err := claim.NewToken(l.project, issue.key, claim.RoleArchitect)
		if err == nil && sandbox.SandboxName(architect) == sandbox.SandboxName(token) {
			return issue.key
		}
	}
	return ""
}

// adoptionsOf is each adoption of a working copy of issue the runtime was asked for, in order
// (adoptionOf).
func (l *taskCopiesLive) adoptionsOf(issue string) []copiesAdoption {
	var of []copiesAdoption
	for _, call := range l.rt.CallsOf("AdoptWorkingCopy") {
		if l.adoptionOf(call.Locator.Claim, call.Branch) == issue {
			of = append(of, copiesAdoption{Locator: call.Locator, Branch: call.Branch})
		}
	}
	return of
}

// copiesAdoption is one adoption of a working copy: the process it went to, and the branch it
// names, "" for the issue's own.
type copiesAdoption struct {
	Locator runtime.Locator
	Branch  string
}

// copiesAbbrev is a commit's first twelve characters, as a note prints it.
func copiesAbbrev(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
