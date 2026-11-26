//go:build e2e

// The live proof's driver of a plan of several tasks: the owner of an issue gives Legion a plan
// whose tasks need each other, and the whole daemon starts each task once the tasks it needs are
// done, two at one time where none needs the other, holds a step whose agent works a step of
// another task until that agent is free, takes a later version of the plan by the state of each
// task, and refuses a plan whose needs form a cycle; a person reads each task, its needs and the
// state of each planned step with the real `dispatch steps`. scripts/e2e/tasks.sh builds this
// driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The rig is the package's rig of a plan of several tasks (tasksRig, tasks_test.go), over four
// issues of one project on the real Dispatch this checkout builds (startHandoffDispatch), with
// dispatch_url at it: register_plan reads each version of plan.md from Dispatch with the daemon's
// own client, and each step record and the plan node go through the daemon's outbox to the real
// routes. GitHub is the rig's fake API over a real bare repository, so each branch, join and pull
// request of a task is one git can read; each review is a person's on the run's broker and the
// merge GitHub's envelope there. The reader of the steps is the real CLI as a person of the
// project, with the personal token Dispatch minted for that person (stepRecordsLive).
//
// This release runs one agent of each role for an issue: the steps of one role share one key in
// every task, as the rig's plans write them. TASKS_CONTROL names one control, a change of the
// driver's input that one check must fail on (tasksControls); a run with a control never passes.
package daemon

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// tasksControlEnv names the control a run makes; unset or empty, it makes none.
const tasksControlEnv = "TASKS_CONTROL"

// tasksControls is each control, by its value of tasksControlEnv, and the check it must fail.
var tasksControls = map[string]string{
	// c needs nothing and d needs a, b and c: c starts at the gate beside a and b. The plan's own
	// control, c's needs taken out alone, leaves b needed by no task, a second task whose work
	// would merge, which this release refuses.
	"c-needs-nothing": "tasks-at-one-time",
	// the example issue's plan rows deleted once its plan is given: tasks-at-one-time reads no
	// task and ends UNKNOWN, never ok.
	"rows-deleted": "tasks-at-one-time",
	// task e deleted from the plan node of the issue in Dispatch's store.
	"node-task-deleted": "steps-read",
	// y's first step a planner of its own, before its implementer of the key k: y's first step
	// gets an agent of its own and starts at the gate beside x's. The plan's own control, the key
	// taken off y's implementer, gives the issue a second implementer, which this release refuses.
	"y-own-agent": "step-waits-for-agent",
	// version 3 changes only the waiting g, which Legion takes.
	"v3-changes-g": "later-version",
	// the cycle cut: a needs nothing.
	"cycle-cut": "cycle-refused",
}

// tasksLiveTitles is the title of each issue of the run: the example plan's, the agent's, the
// later version's and the cycle's.
var tasksLiveTitles = []string{"One change split in several tasks", "Parsers queue behind their agent",
	"Widget storage replanned midway", "Circular needs refused upfront"}

// tasksLive is one run of the proof: the rig over the run's four issues, the person's CLI over
// the same rig (reads), the control the run makes, and why the running check read nothing.
type tasksLive struct {
	*tasksRig
	reads   *stepRecordsLive
	control string
	unknown string
}

// TestTasksLive drives the proof's checks against one booted daemon and one Dispatch, over four
// issues of one project: the first walks the example plan to its merge and is read with `dispatch
// steps`, the second runs two tasks whose first steps share one agent, the third is given two
// later versions while its first task runs, and the fourth is given a plan whose needs form a
// cycle.
func TestTasksLive(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (newTasksLive).
	l := newTasksLive(t)
	example, agents, change, cycle := l.issues[0].key, l.issues[1].key, l.issues[2].key, l.issues[3].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s, %s and %s; daemon at dispatch_url %s",
		l.dispatch.base, l.cfg.Project, liveRepo, example, agents, change, cycle, l.cfg.DispatchURL)
	if l.control != "" {
		l.note("control %s=%s: the check %s must fail", tasksControlEnv, l.control, tasksControls[l.control])
	}
	l.check("tasks-at-one-time", func() {
		l.on(example)
		l.tasksAtOneTime()
	})
	l.check("steps-read", func() {
		l.on(example)
		l.stepsRead()
	})
	l.check("step-waits-for-agent", func() {
		l.on(agents)
		l.stepWaitsForAgent()
	})
	l.check("later-version", func() {
		l.on(change)
		l.laterVersion()
	})
	l.check("cycle-refused", func() {
		l.on(cycle)
		l.cycleRefused()
	})
}

// newTasksLive is the run around the checks: the broker the script names (or a skip naming the
// script), the control it names, the checkout its script runs from and bun on PATH
// (liveCLICheckout), the rig over the run's issues on a Dispatch this checkout builds, and the
// person's CLI with a personal token Dispatch minted for that person.
func newTasksLive(t *testing.T) *tasksLive {
	t.Helper()
	fixtureenv.Live(t)
	proof := newLiveProof(t, "scripts/e2e/tasks.sh")
	control := os.Getenv(tasksControlEnv)
	if _, known := tasksControls[control]; control != "" && !known {
		t.Fatalf("%s=%q names no control; the controls are %v", tasksControlEnv, control, slices.Sorted(maps.Keys(tasksControls)))
	}
	root := liveCLICheckout(t)
	rig := newTasksRigWith(t, proof, startHandoffDispatch(t, root).scratchDispatch, tasksLiveTitles...)
	l := &tasksLive{tasksRig: rig, reads: &stepRecordsLive{stepRecordsRig: rig.stepRecordsRig, root: root, cliHome: t.TempDir()}, control: control}
	l.reads.mintReader("tasks-reader")
	return l
}

// check runs one of the proof's checks between its `== <name>` line and its one line of outcome:
// `ok <name> (<s>s)`; `UNKNOWN <name> (<s>s): <why>` when it read nothing to judge; or `FAIL
// <name> (<s>s)` when it failed the test. Either of the last two ends the run. The check that the
// run's control names must fail: one that passes fails the run, naming the control.
func (l *tasksLive) check(name string, body func()) {
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
	if l.control != "" && tasksControls[l.control] == name {
		l.t.Fatalf("control %s=%s: the check %s passed, and the control must make it fail", tasksControlEnv, l.control, name)
	}
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// nothingRead ends the running check as UNKNOWN: it read nothing to judge, as format says.
func (l *tasksLive) nothingRead(format string, args ...any) {
	l.t.Helper()
	l.unknown = fmt.Sprintf(format, args...)
	l.t.Fatal("UNKNOWN: " + l.unknown)
}

// controlled is form with each old text of pairs (old, new, old, new, ...) replaced by its new,
// when the run's control is control, and form as it is otherwise. Each old text must stand in form
// exactly once, so a form that changed fails the run rather than running the control on the plan
// it was meant to change.
func (l *tasksLive) controlled(control, form string, pairs ...string) string {
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

// tasksNow is each task of the walk's issue as plan_tasks holds it, printed with when. A read of
// no task is no observation: the check ends UNKNOWN.
func (l *tasksLive) tasksNow(when string) map[string]tasksRow {
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
		stands = append(stands, at)
	}
	l.note("%s %s: %s", l.key, when, strings.Join(stands, "; "))
	return rows
}

// tasksAtOneTime is the check tasks-at-one-time, on the example plan (examplePlan): once the gate
// opens, a and b both run, each at its first step, while c, d and e wait; c waits until b is done,
// d until a and c are, e until d is, and each starts once they are; e's pull request merges and
// the issue is done. b's implementer step waits for the one implementer while a's works.
func (l *tasksLive) tasksAtOneTime() {
	l.t.Helper()
	l.givePlanText(l.controlled("c-needs-nothing", examplePlan,
		`{#c needs="%[1]sb" `, `{#c `, `{#d needs="%[1]sa %[1]sc" `, `{#d needs="%[1]sa %[1]sb %[1]sc" `))
	if l.control == "rows-deleted" {
		l.deletePlanRows()
		l.note("control rows-deleted: deleted %s's rows from plan_steps, plan_tasks and issue_plans", l.key)
	}
	l.tasksNow("once its plan was given")
	l.firstStep()
	l.waitAt("a", 1)
	l.waitAt("b", 1)
	l.tasksNow("once its gate opened")
	l.stillWaiting("both a and b run", "c", "d", "e")
	l.note("a and b run at one time, before either is done; c, d and e wait")

	l.planStep("b")
	l.waitAgent("b", 2)
	l.implementStep("a", "main")
	l.waitAt("b", 2)
	l.reviewStep("a")
	l.waitDone("a")
	l.tasksNow("once a was done")
	l.stillWaiting("a is done and b is not", "c", "d", "e")

	l.implementStep("b", "main")
	l.stillWaiting("b reviews", "c", "d", "e")
	l.reviewStep("b")
	l.waitDone("b")
	l.waitAt("c", 1)
	l.tasksNow("once b was done")
	l.stillWaiting("c runs", "d", "e")

	l.implementStep("c", l.branchOf("b"))
	l.stillWaiting("c reviews", "d", "e")
	l.reviewStep("c")
	l.waitDone("c")
	l.waitAt("d", 1)
	l.tasksNow("once c was done")
	l.stillWaiting("d runs", "e")

	l.implementStep("d", l.branchOf("d")+claim.JoinBranchSuffix)
	l.stillWaiting("d reviews", "e")
	l.reviewStep("d")
	l.waitDone("d")
	l.waitAt("e", 1)
	l.tasksNow("once d was done")

	l.implementStep("e", "main")
	l.testStep("e")
	l.retroAndMerge("e")
	l.tasksNow("once e's pull request merged")
	l.note("c started once b was done, d once a and c were, e once d was; %s is done", l.key)
}

// exampleTaskLines is what `dispatch steps` prints of each task of the example plan once its
// issue is done: the task, its needs, and each planned step with its key, passed.
var exampleTaskLines = []string{
	"task a needs []: 1 implementer:i passed, 2 reviewer:r passed",
	"task b needs []: 1 planner:p passed, 2 implementer:i passed, 3 reviewer:r passed",
	"task c needs [b]: 1 implementer:i passed, 2 reviewer:r passed",
	"task d needs [a,c]: 1 implementer:i passed, 2 reviewer:r passed",
	"task e needs [d]: 1 implementer:i passed, 2 tester:t passed",
}

// stepsRead is the check steps-read: `dispatch steps --issue <KEY>` as the person, on the issue of
// the example plan once it is done, prints the header of its five tasks, then each task of the
// plan in id order with its needs and the state of each planned step (exampleTaskLines). The step
// records ride the outbox, so the read is made again until it holds them or the wait ends.
func (l *tasksLive) stepsRead() {
	l.t.Helper()
	if l.control == "node-task-deleted" {
		l.dropFromPlanNode("e")
	}
	var out string
	var lines []string
	tasks := 0
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		out, code = l.reads.steps()
		if code != 0 {
			l.t.Fatalf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
		}
		lines, tasks = taskLinesOf(l.key, out)
		if (slices.Equal(lines, exampleTaskLines) && tasks == len(exampleTaskLines)) || time.Now().After(deadline) {
			break
		}
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
	if len(lines) == 0 {
		l.nothingRead("`dispatch steps --issue %s` printed no line of a task of its plan", l.key)
	}
	for _, want := range exampleTaskLines {
		if !slices.Contains(lines, want) {
			l.t.Fatalf("`dispatch steps --issue %s` prints no line %q; its task lines are %q", l.key, want, lines)
		}
	}
	if !slices.Equal(lines, exampleTaskLines) || tasks != len(exampleTaskLines) {
		l.t.Fatalf("`dispatch steps --issue %s` prints %d tasks and the task lines %q, want %d tasks and %q", l.key, tasks, lines, len(exampleTaskLines), exampleTaskLines)
	}
}

// taskLinesOf is each line of a task of the plan that out, what `dispatch steps --issue <key>`
// printed, holds (`task <id> needs …: …`), in order, and the count of tasks its header names (0
// where out has no header).
func taskLinesOf(key, out string) ([]string, int) {
	var lines []string
	tasks := 0
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 {
			var steps int
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, key+": "), "%d steps in %d tasks", &steps, &tasks); err != nil {
				tasks = 0
			}
			continue
		}
		if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "task" && fields[2] == "needs" {
			lines = append(lines, line)
		}
	}
	return lines, tasks
}

// dropFromPlanNode deletes task from the tasks of the walk's issue's plan node in Dispatch's own
// store, the control of steps-read: the plan a person reads then lacks it.
func (l *tasksLive) dropFromPlanNode(task string) {
	l.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, l.dispatch.database)
	if err != nil {
		l.t.Fatalf("connect to Dispatch's store: %v", err)
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, `update workflow_nodes n set attributes = jsonb_set(n.attributes, '{tasks}',
			(select coalesce(jsonb_agg(held), '[]'::jsonb) from jsonb_array_elements(n.attributes->'tasks') held where held->>'id' <> $2))
		from workflow_tuples t
		where t.to_kind = 'issue' and t.to_id = $1 and t.kind = 'part_of' and t.from_kind = 'plan' and t.asserted
			and n.kind = t.from_kind and n.id = t.from_id and n.attributes->'tasks' @> jsonb_build_array(jsonb_build_object('id', $2::text))`,
		l.key, task)
	if err != nil {
		l.t.Fatalf("delete task %s from the plan node of %s: %v", task, l.key, err)
	}
	if tag.RowsAffected() != 1 {
		l.t.Fatalf("the delete of task %s changed %d plan nodes of %s, want 1", task, tag.RowsAffected(), l.key)
	}
	l.note("control: deleted task %s from the plan node of %s in Dispatch's store", task, l.key)
}

// stepWaitsForAgent is the check step-waits-for-agent, on the plan of x and y whose first steps
// are of the one key k (agentsPlan): once the gate opens, x's step runs and y's waits for k, with
// no start of its own; once x's step ends, y's step starts, in the session of x's step, and the
// implementer was launched once.
func (l *tasksLive) stepWaitsForAgent() {
	l.t.Helper()
	l.givePlanText(l.controlled("y-own-agent", agentsPlan,
		`{#y steps="implementer:k reviewer:r"}`, `{#y steps="planner:p implementer:k reviewer:r"}`))
	l.tasksNow("once its plan was given")
	l.firstStep()
	l.waitAt("x", 1)
	l.waitAgent("y", 1)
	l.tasksNow("once its gate opened")
	k := l.worker(claim.RoleImplementer)
	l.note("k's session %s works x's step; y's step waits for k", k.session)
	l.implementStep("x", "main")
	l.waitAt("y", 1)
	l.tasksNow("once x's step ended")
	l.implementStep("y", "main")
	if got := l.d.claim(k.token).Session; got != k.session {
		l.t.Fatalf("the claim of k holds the session %q after y's step, want %q, the session of x's step", got, k.session)
	}
	if launches := l.launchesOfRole(claim.RoleImplementer); len(launches) != 1 {
		l.t.Fatalf("the implementer of %s was launched %v, want once: y's step in the session of x's", l.key, launches)
	}
	l.note("y's step started once x's ended, in k's one session %s; the implementer was launched once", k.session)
}

// The blocks of the plan of later-version, each need written as examplePlan's (%[1]s).
const (
	changeA = ":::plan_task{#a steps=\"implementer:i reviewer:r\"}\nChange the store of the widget.\n:::\n"
	changeG = ":::plan_task{#g needs=\"%[1]sa\" steps=\"implementer:i\"}\nMove the readers of the store.\n:::\n"
	changeK = ":::plan_task{#k needs=\"%[1]sa\" steps=\"implementer:i tester:t\"}\nCheck the moved store.\n:::\n"
	changeZ = ":::plan_task{#z needs=\"%[1]sg %[1]sk\" steps=\"implementer:i\"}\nJoin the readers and the check.\n:::\n"
	// The blocks version 2 adds and changes: f needs nothing and z needs it, g gains a reviewer,
	// and z needs k no more.
	changeF  = ":::plan_task{#f steps=\"implementer:i\"}\nAdd a cache to the store.\n:::\n"
	changeG2 = ":::plan_task{#g needs=\"%[1]sa\" steps=\"implementer:i reviewer:r\"}\nMove the readers of the store.\n:::\n"
	changeZ2 = ":::plan_task{#z needs=\"%[1]sg %[1]sf\" steps=\"implementer:i\"}\nJoin the readers and the check.\n:::\n"
	// The block of a in version 3, which changes the running a, and of g in its control.
	changeA3 = ":::plan_task{#a steps=\"implementer:i tester:t\"}\nChange the store of the widget.\n:::\n"
	changeG3 = ":::plan_task{#g needs=\"%[1]sa\" steps=\"implementer:i tester:t\"}\nMove the readers of the store.\n:::\n"
)

// runningRefusal is the one reason Legion refuses a version that changes the running a with.
var runningRefusal = api.PlanReason{Task: "a", Reason: "task a is running: stop it with stop_task before you change it."}

// laterVersion is the check later-version: of a (`implementer reviewer`, running), g
// (`implementer`, needs a), k (needs a) and z (needs g and k), version 2 adds f (needing nothing,
// which z now needs), changes g's steps to `implementer reviewer` and removes k: Legion takes it, f
// runs, g waits with its two steps, and k is gone, never started. Version 3, which changes the
// running a, is refused 409 PLAN_REFUSED with that one reason, and a keeps the steps it started
// with.
func (l *tasksLive) laterVersion() {
	l.t.Helper()
	l.givePlanText(changeA + "\n" + changeG + "\n" + changeK + "\n" + changeZ)
	l.tasksNow("at version 1")
	l.firstStep()
	l.waitAt("a", 1)
	l.tasksNow("once its gate opened")

	plan, version := l.planVersion(changeA + "\n" + changeF + "\n" + changeG2 + "\n" + changeZ2)
	status, body := l.registerPlan(plan, version)
	l.note("register_plan of version %d (adds f, changes g, removes k): %d %s", version, status, strings.TrimSpace(body))
	if status != http.StatusOK {
		l.t.Fatalf("register_plan of version %d of %s = %d %s, want 200", version, plan, status, body)
	}
	l.waitTask("f", "running, the task version 2 added", func(row tasksRow) bool { return row.state == string(record.TaskRunning) })
	rows := l.tasksNow("once version 2 was taken")
	if _, held := rows["k"]; held {
		l.t.Fatalf("the plan of %s holds k after version 2 removed it: %+v", l.key, rows["k"])
	}
	if row := rows["g"]; row.state != string(record.TaskWaiting) {
		l.t.Fatalf("task g of %s stands %+v after version 2, want waiting for a", l.key, row)
	}
	if got := l.stepWords("g"); !slices.Equal(got, []string{"implementer:i", "reviewer:r"}) {
		l.t.Fatalf("task g's steps after version 2 = %v, want implementer:i reviewer:r", got)
	}
	if starts := l.startsOf("k"); starts != 0 {
		l.t.Fatalf("task k of %s has %d step starts, want none: version 2 removed it before it started", l.key, starts)
	}
	l.note("f runs, g waits with the steps %v, and k is gone and never started", l.stepWords("g"))

	third := changeA3 + "\n" + changeF + "\n" + changeG2 + "\n" + changeZ2
	if l.control == "v3-changes-g" {
		third = changeA + "\n" + changeF + "\n" + changeG3 + "\n" + changeZ2
		l.note("control v3-changes-g: version 3 changes only the waiting g, to implementer:i tester:t")
	}
	plan, version = l.planVersion(third)
	status, body = l.registerPlan(plan, version)
	l.note("register_plan of version %d: %d %s", version, status, strings.TrimSpace(body))
	var refusal api.PlanRefusal
	if status != http.StatusConflict {
		l.t.Fatalf("register_plan of version %d of %s = %d %s, want 409 PLAN_REFUSED with the one reason %+v", version, plan, status, body, runningRefusal)
	}
	decodeInto(l.t, "the refusal of register_plan", body, &refusal)
	if len(refusal.Reasons) == 0 {
		l.nothingRead("the refusal of version %d names no reason: %s", version, body)
	}
	if refusal.Code != "PLAN_REFUSED" || !slices.Equal(refusal.Reasons, []api.PlanReason{runningRefusal}) {
		l.t.Fatalf("register_plan of version %d = %d %s, want 409 PLAN_REFUSED with the one reason %+v", version, status, body, runningRefusal)
	}
	if got := l.stepWords("a"); !slices.Equal(got, []string{"implementer:i", "reviewer:r"}) {
		l.t.Fatalf("task a's steps after version %d was refused = %v, want the steps it started with", version, got)
	}
	l.note("version %d was refused with the one reason of a; a keeps the steps implementer:i reviewer:r", version)
}

// planVersion writes form as the next version of the walk's issue's plan.md, each %[1]s in it the
// reference to a block of that document (givePlanText), and answers the document's id and the
// version Dispatch wrote. An upload under a name the issue already holds writes that document's
// next version.
func (l *tasksLive) planVersion(form string) (string, int) {
	l.t.Helper()
	var written struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	l.person(http.MethodPost, "/api/v1/issues/"+l.key+"/artifacts", map[string]string{"name": "plan.md",
		"content": fmt.Sprintf(form, "dispatch://"+l.key+"/artifact/plan-md#b-")}, http.StatusCreated, &written)
	if written.Artifact.ID == "" || written.Version.Number < 2 {
		l.t.Fatalf("Dispatch answered the upload of a later plan.md on %s with %+v, want a document's version above 1", l.key, written)
	}
	return written.Artifact.ID, written.Version.Number
}

// stepWords is each step of task of the walk's issue's generation, in order, as plan_steps holds
// it: `role`, or `role:key`.
func (l *tasksLive) stepWords(task string) []string {
	l.t.Helper()
	rows, err := l.st.Pool().Query(context.Background(), `select role || coalesce(':' || nullif(agent_key, ''), '') from plan_steps
		where issue = $1 and task = $2 and generation = (select generation from issues where key = $1) order by position`, l.key, task)
	if err != nil {
		l.t.Fatalf("read the steps of task %s of %s: %v", task, l.key, err)
	}
	defer rows.Close()
	var words []string
	for rows.Next() {
		var word string
		if err := rows.Scan(&word); err != nil {
			l.t.Fatal(err)
		}
		words = append(words, word)
	}
	if err := rows.Err(); err != nil {
		l.t.Fatal(err)
	}
	return words
}

// startsOf is how many rows of step_starts task of the walk's issue has.
func (l *tasksLive) startsOf(task string) int {
	l.t.Helper()
	var starts int
	if err := l.st.Pool().QueryRow(context.Background(), `select count(*) from step_starts where issue = $1 and task = $2`, l.key, task).Scan(&starts); err != nil {
		l.t.Fatalf("count the step starts of task %s of %s: %v", task, l.key, err)
	}
	return starts
}

// cyclePlan is two tasks that need each other, each need written as examplePlan's.
const cyclePlan = `:::plan_task{#a needs="%[1]sb" steps="implementer:i reviewer:r"}
Change the parser of the widget.
:::

:::plan_task{#b needs="%[1]sa" steps="implementer:i"}
Change the store of the widget.
:::
`

// cycleReason is the reason that names the cycle of cyclePlan.
var cycleReason = api.PlanReason{Task: "a", Reason: "task a needs itself: a needs b, b needs a."}

// cycleRefused is the check cycle-refused: register_plan of a plan whose tasks need each other, on
// an issue at admitted, is refused 409 PLAN_REFUSED, and among its reasons is the one that names
// the cycle (cycleReason); the issue holds no plan row.
func (l *tasksLive) cycleRefused() {
	l.t.Helper()
	form := l.controlled("cycle-cut", cyclePlan, `{#a needs="%[1]sb" `, `{#a `)
	plan := l.document("plan.md", fmt.Sprintf(form, "dispatch://"+l.key+"/artifact/plan-md#b-"))
	status, body := l.registerPlan(plan, 1)
	l.note("register_plan of the plan given: %d %s", status, strings.TrimSpace(body))
	if status != http.StatusConflict {
		l.t.Fatalf("register_plan of a cyclic plan on %s = %d %s, want 409 PLAN_REFUSED naming the cycle %+v", l.key, status, body, cycleReason)
	}
	var refusal api.PlanRefusal
	decodeInto(l.t, "the refusal of register_plan", body, &refusal)
	if len(refusal.Reasons) == 0 {
		l.nothingRead("the refusal of the cyclic plan names no reason: %s", body)
	}
	if refusal.Code != "PLAN_REFUSED" || !slices.Contains(refusal.Reasons, cycleReason) {
		l.t.Fatalf("register_plan of a cyclic plan = %d %s, want 409 PLAN_REFUSED holding the reason %+v", status, body, cycleReason)
	}
	if rows := l.planRows(); rows != 0 {
		l.t.Fatalf("the refused plan left %d plan rows on %s, want none", rows, l.key)
	}
	l.note("the cyclic plan was refused naming the cycle; %s holds no plan rows", l.key)
}
