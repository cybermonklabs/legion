//go:build e2e

// The live proof's driver of an owner's plan: the owner of an issue writes its plan.md through the
// real `dispatch` CLI and gives it to Legion with register_plan, and the whole daemon runs the steps
// that plan names, in the order it names them and no other, then the retro and the merge, while an
// issue whose owner gives no plan runs the project's default steps. scripts/e2e/owner-steps.sh
// builds this driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// Nothing of the plan's path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server. The daemon is the
// whole daemon (run, through startDaemonWithin) with dispatch_url at that server, so register_plan
// reads the plan's version from Dispatch with the daemon's own client, and each step record goes
// through the daemon's outbox to the real route. The issues walk as the package's whole-daemon tests
// of an owner's plan walk them (ownerStepsRig, owner_steps_rig_test.go): each fact of GitHub reaches
// the daemon through its own reads of a fake GitHub API and its own intake, never through a write to
// its store. The owner writes with the real CLI under bun in its own session, and the reader of the
// steps is the same CLI as a person of the project, with the personal token Dispatch minted for that
// person (stepRecordsLive).
//
// What is stood in for lies outside the plan: GitHub's REST API (stepRecordsGitHub), the Apps'
// tokens, Envoy's listener, and the panes, which the package's fake runtime launches and the driver
// plays.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// ownerStepsPlan is the plan the owner of the driver's first issue gives: one task a, whose steps
// are the implementer and then the reviewer.
var ownerStepsPlan = []string{"implementer", "reviewer"}

// ownerStepsAnyOrder is the plan the owner of the driver's fifth issue gives: one task a whose
// tester runs after its reviewer, in another order than the default steps'.
var ownerStepsAnyOrder = []string{"implementer", "reviewer", "tester"}

// ownerStepsLive is one run of the proof: the rig over five issues of one project, and the
// person's CLI over the same rig (reads).
type ownerStepsLive struct {
	*ownerStepsRig
	reads *stepRecordsLive
}

// TestOwnerStepsLive drives the proof's checks against one booted daemon and one Dispatch, over
// five issues of one project: the first walks its owner's plan `implementer reviewer`, the second
// the default steps, the third the default steps with its plan rows deleted while its planner works,
// the fourth is given a plan Legion refuses and then a plan whose rows are deleted before its
// gate opens, and the fifth is refused the plan `tester implementer` and then walks its owner's
// plan `implementer reviewer tester`.
func TestOwnerStepsLive(t *testing.T) {
	l := newOwnerStepsLive(t, "scripts/e2e/owner-steps.sh", 5)
	planned, byDefault, rowsDeleted, unplanned, anyOrder := l.issues[0].key, l.issues[1].key, l.issues[2].key, l.issues[3].key, l.issues[4].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s, %s, %s and %s; daemon at dispatch_url %s",
		l.dispatch.base, l.cfg.Project, liveRepo, planned, byDefault, rowsDeleted, unplanned, anyOrder, l.cfg.DispatchURL)
	var plan string
	l.check("plan-written", func() {
		l.on(planned)
		plan = l.planWritten(ownerStepsPlan)
	})
	l.check("plan-registered", func() { l.planRegistered(plan, ownerStepsPlan) })
	l.check("walk-implementer-reviewer", func() {
		if err := l.ownersPlanWalked(ownerStepsPlan); err != nil {
			l.t.Fatal(err)
		}
	})
	l.check("steps-read", func() { l.stepsRead(planned, "a", ownerStepsPlan) })
	l.check("walk-default-steps", func() {
		l.on(byDefault)
		l.walkDefault()
		if err := l.defaultRowsHold(); err != nil {
			l.t.Fatal(err)
		}
		l.stepsRead(byDefault, "default", ownerStepsDefault)
	})
	l.check("control-default-rows-deleted", func() {
		l.on(rowsDeleted)
		l.walkDefaultWithRowsDeleted()
		err := l.defaultRowsHold()
		if want := rowsDeleted + "'s issue_plans rows are []"; err == nil || !strings.Contains(err.Error(), want) {
			l.t.Fatalf("control: the check of the default steps' rows on %s, whose rows were deleted, says %v, want it to say %q", rowsDeleted, err, want)
		}
		l.note("control: the check of the default steps' rows fails on %s: %v", rowsDeleted, err)
	})
	l.check("control-refused", func() {
		l.on(unplanned)
		l.refused()
	})
	l.check("control-no-plan-rows", func() {
		l.on(unplanned)
		l.noPlanRows()
	})
	l.check("control-tester-first", func() {
		l.on(anyOrder)
		l.note("register_plan of `tester implementer` on %s: 409 %s; %s holds no plan rows", anyOrder, strings.TrimSpace(l.testerFirstRefused()), anyOrder)
	})
	l.check("walk-any-order", func() {
		l.on(anyOrder)
		l.givePlan(ownerStepsAnyOrder...)
		l.openGate(phase.Implementing)
		l.walkSteps(ownerStepsAnyOrder)
		if err := l.launchedInOrder([]claim.Role{claim.RoleImplementer, claim.RoleReviewer, claim.RoleTester}); err != nil {
			l.t.Fatal(err)
		}
		l.note("%s walked implementing, reviewing, testing, retro and merging to done, launching its implementer, its reviewer and its tester in that order", anyOrder)
		l.stepsRead(anyOrder, "a", ownerStepsAnyOrder)
	})
}

// newOwnerStepsLive is the run a live driver of an owner's plan makes around its checks: the broker
// the script names (or a skip naming script), the checkout its script runs from and bun on PATH
// (liveCLICheckout), the rig over issues issues on a Dispatch this checkout builds, and the person's
// CLI with a personal token Dispatch minted for that person.
func newOwnerStepsLive(t *testing.T, script string, issues int) *ownerStepsLive {
	t.Helper()
	fixtureenv.Live(t)
	proof := newLiveProof(t, script)
	root := liveCLICheckout(t)
	rig := newOwnerStepsRig(t, proof, startHandoffDispatch(t, root).scratchDispatch, issues, config.Project{})
	l := &ownerStepsLive{ownerStepsRig: rig, reads: &stepRecordsLive{stepRecordsRig: rig.stepRecordsRig, root: root, cliHome: t.TempDir()}}
	l.reads.mintReader("owner-steps-reader")
	return l
}

// check runs one of the proof's checks between its `== <name>` line and its one line of outcome,
// `ok <name> (<s>s)`, or `FAIL <name> (<s>s)` when the check failed the test, which ends the run.
func (l *ownerStepsLive) check(name string, body func()) {
	l.t.Helper()
	started := time.Now()
	fmt.Printf("== %s\n", name)
	defer func() {
		if l.t.Failed() {
			fmt.Printf("FAIL %s (%ds)\n", name, int(time.Since(started).Seconds()))
		}
	}()
	body()
	fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
}

// planWritten is the owner's session, the architect's claim of the walk's issue, writing plan.md on
// the issue with one block `:::plan_task{#a steps="<steps>"}` through the real `dispatch artifact`
// command, which prints the document's reference `dispatch://<KEY>/artifact/plan-md`, and the
// daemon's own client reading it back at version 1 with its blocks. The document's id, which
// register_plan takes, is the one the issue's own read of its documents names for that slug
// (GET /api/v1/issues/{key}/summary). It answers that id.
func (l *ownerStepsLive) planWritten(steps []string) string {
	t := l.t
	t.Helper()
	owner := l.worker(claim.RoleArchitect)
	shell := cliShell{t: t, dir: l.reads.cliHome, session: owner.session, base: l.dispatch.base, token: l.dispatch.token}
	out, code := shell.bun(nil, dispatchCLI(l.reads.root), "artifact", "--issue", l.key, "--name", "plan.md", "--content", planOf(steps...))
	printed := string(out.stdout) + string(out.stderr)
	if code != 0 {
		t.Fatalf("`dispatch artifact --issue %s --name plan.md` as the owner's session %s exited %d: %s", l.key, owner.session, code, printed)
	}
	if ref := "dispatch://" + l.key + "/artifact/plan-md"; !strings.Contains(printed, ref) {
		t.Fatalf("`dispatch artifact --issue %s --name plan.md` printed %q, want the reference %s", l.key, printed, ref)
	}
	var summary struct {
		Documents []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"documents"`
	}
	if err := l.personRead("/api/v1/issues/"+l.key+"/summary", &summary); err != nil {
		t.Fatalf("read the documents of %s: %v", l.key, err)
	}
	id := ""
	for _, document := range summary.Documents {
		if document.Slug == "plan-md" {
			id = document.ID
		}
	}
	if id == "" {
		t.Fatalf("the documents of %s are %+v, want one of the slug plan-md", l.key, summary.Documents)
	}
	read, err := dispatch.New(l.dispatch.base, dispatch.BearerTransport(l.dispatch.token, nil)).PlanDocument(context.Background(), id, 1)
	if err != nil {
		t.Fatalf("read %s at version 1 as the daemon: %v", id, err)
	}
	want := strings.Join(steps, " ")
	if len(read.Blocks) != 1 || read.Blocks[0].Type != "plan_task" || read.Blocks[0].ID != "a" || read.Blocks[0].Attributes["steps"] != want || read.IssueKey != l.key {
		t.Fatalf("%s at version 1 reads %+v, want one plan_task block a with steps %q on %s", id, read, want, l.key)
	}
	l.note("the owner's session %s wrote plan.md (%s) on %s with `dispatch artifact`; version 1 reads one plan_task block a, steps %q", owner.session, id, l.key, want)
	return id
}

// planRegistered is the owner giving version 1 of plan, before the gate opens, with
// POST /legion/v1/plans/register under the architect's claim grant: 200, the plan at version 1 with
// one task a naming steps.
func (l *ownerStepsLive) planRegistered(plan string, steps []string) {
	t := l.t
	t.Helper()
	if got := string(l.d.state().Issues[l.key].Phase); got != string(phase.Admitted) {
		t.Fatalf("%s stands in %s when its owner gives its plan, want admitted, before its gate opens", l.key, got)
	}
	status, body := l.registerPlan(plan, 1)
	var taken api.PlanRegisterResponse
	if status != http.StatusOK {
		t.Fatalf("register_plan of %s version 1 = %d %s, want 200", plan, status, body)
	}
	decodeInto(t, "the answer of register_plan", body, &taken)
	if taken.Plan.Version != 1 || len(taken.Plan.Tasks) != 1 || taken.Plan.Tasks[0].ID != "a" || !slices.Equal(taken.Plan.Tasks[0].Steps, steps) {
		t.Fatalf("register_plan answered %s, want version 1 with one task a naming %v", body, steps)
	}
	l.note("POST /legion/v1/plans/register with the architect's grant, version 1: 200 %s", strings.TrimSpace(body))
}

// ownersPlanWalked is walk-implementer-reviewer: the issue's gate opens, and it walks the steps of
// its owner's plan (steps), then its retro and its merge, to done, with no planner and no tester
// launched at any point. The error names what does not hold.
func (l *ownerStepsLive) ownersPlanWalked(steps []string) error {
	l.t.Helper()
	first := l.firstStep()
	if want := stepPhase(l.t, steps[0]); first != want {
		return fmt.Errorf("%s's gate opened and its first step is %s, want %s, the first step of its owner's plan (planner launched %v)",
			l.key, first, want, l.launchesOfRole(claim.RolePlanner))
	}
	l.walkSteps(steps)
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleTester} {
		if launches := l.launchesOfRole(role); len(launches) != 0 {
			return fmt.Errorf("%s's %s was launched %v on the plan %v, want never", l.key, role, launches, steps)
		}
	}
	l.note("%s walked %s, then retro and merging, to done; no planner and no tester was launched", l.key, strings.Join(steps, ", "))
	return nil
}

// stepsRead runs `dispatch steps --issue <issue>` as the person and holds that it prints task's
// steps, each step of steps at its place, then the retro and the merge, each ended passed, and no
// other run.
func (l *ownerStepsLive) stepsRead(issue, task string, steps []string) {
	t := l.t
	t.Helper()
	l.on(issue)
	var want []stepWant
	types := map[string]string{"planner": "planning", "implementer": "implementation", "tester": "testing", "reviewer": "review", "acceptance:owner": "review"}
	for place, step := range steps {
		role, _, _ := strings.Cut(step, ":")
		want = append(want, stepWant{kind: "step", step: place + 1, attempt: 1, role: role, typ: types[step], result: "passed"})
	}
	want = append(want, stepWant{kind: "retro", attempt: 1, role: "implementer", result: "passed"},
		stepWant{kind: "merge", attempt: 1, role: "merger", result: "passed"})
	var out string
	var err error
	// The records ride the outbox, so `dispatch steps` holds them a moment after the walk ends.
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		out, code = l.reads.steps()
		if code != 0 {
			t.Fatalf("`dispatch steps --issue %s` exited %d: %s", issue, code, out)
		}
		if err = taskLinesHold(issue, out, task, want); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("`dispatch steps --issue %s` printed\n%s\nand the read does not hold task %s's walk: %v", issue, out, task, err)
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", issue)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
}

// taskLinesHold says whether out, what `dispatch steps --issue <issue>` printed, holds a header of
// one task and exactly the runs of want, each of task, ended with its result and with an agent. The
// line of the task's needs and planned steps before its runs is no run, and parseSteps passes it
// over.
func taskLinesHold(issue, out, task string, want []stepWant) error {
	printed, err := parseSteps(issue, out)
	if err != nil {
		return err
	}
	for _, w := range want {
		got := findLine(printed.lines, w)
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", lineName(w))
		case got.task != task || got.role != w.role || got.typ != printedType(w) || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task %s, role %s, type %s and an agent", lineName(w), got.text, task, w.role, printedType(w))
		case !printedMoment(got.ended) || got.result != w.result:
			return fmt.Errorf("%s prints %q: want an end and result %s", lineName(w), got.text, w.result)
		}
	}
	if len(printed.lines) != len(want) || printed.tasks != 1 {
		return fmt.Errorf("the header says %d steps in %d tasks and %d lines follow it, want %d steps in 1 tasks", printed.steps, printed.tasks, len(printed.lines), len(want))
	}
	return nil
}

// walkDefault is the walk of an issue whose owner gives no plan: its gate opens, and it walks the
// default steps, then its retro and its merge, to done.
func (l *ownerStepsLive) walkDefault() {
	l.t.Helper()
	l.openGate(phase.Planning)
	l.walkSteps(ownerStepsDefault)
	l.note("%s, given no plan, walked planning, implementing, testing, reviewing, accepting (the architect's sign-off), retro and merging to done", l.key)
}

// walkDefaultWithRowsDeleted is walkDefault with the issue's plan rows deleted from the daemon's
// store once its planner has taken its task and before it completes: the rest of the walk is
// today's line of an issue with no plan rows.
func (l *ownerStepsLive) walkDefaultWithRowsDeleted() {
	l.t.Helper()
	l.openGate(phase.Planning)
	planner := l.worker(claim.RolePlanner)
	planner.take()
	l.deletePlanRows()
	l.note("deleted %s's rows from plan_steps, plan_tasks and issue_plans while its planner works", l.key)
	l.finish(planner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
	for place, step := range ownerStepsDefault[1:] {
		next := phase.Retro
		if place+2 < len(ownerStepsDefault) {
			next = stepPhase(l.t, ownerStepsDefault[place+2])
		}
		l.runStep(step, answerPass, next)
	}
	l.retro()
	l.merge()
	l.note("%s walked today's line to done", l.key)
}

// defaultRowsHold says whether the walk's issue, done, holds the rows of the default steps: one
// plan version 0 of the source default, its task default done, a plan_steps row at each place 1 to
// 5 naming the role of that default step, and one start of each in step_starts. The error names the
// first that does not hold.
func (l *ownerStepsLive) defaultRowsHold() error {
	t := l.t
	t.Helper()
	ctx := context.Background()
	pool := l.st.Pool()
	var versions []string
	rows, err := pool.Query(ctx, "select version || ' ' || source from issue_plans where issue = $1 order by version", l.key)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	if !slices.Equal(versions, []string{"0 default"}) {
		return fmt.Errorf("%s's issue_plans rows are %v, want one version 0 of the source default", l.key, versions)
	}
	var state string
	if err := pool.QueryRow(ctx, "select state from plan_tasks where issue = $1 and task = 'default'", l.key).Scan(&state); err != nil {
		return fmt.Errorf("%s has no plan_tasks row of the task default: %v", l.key, err)
	}
	if state != "done" {
		return fmt.Errorf("%s's task default is %s, want done", l.key, state)
	}
	var places []string
	rows, err = pool.Query(ctx, "select position || ' ' || role from plan_steps where issue = $1 and task = 'default' order by position", l.key)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		places = append(places, p)
	}
	rows.Close()
	if want := []string{"1 planner", "2 implementer", "3 tester", "4 reviewer", "5 acceptance"}; !slices.Equal(places, want) {
		return fmt.Errorf("%s's plan_steps of task default are %v, want %v", l.key, places, want)
	}
	starts := l.stepStarts()
	for place := 1; place <= len(ownerStepsDefault); place++ {
		prefix := fmt.Sprintf("default step %d starts 1 ", place)
		if !slices.ContainsFunc(starts, func(s string) bool { return strings.HasPrefix(s, prefix) }) {
			return fmt.Errorf("%s's step starts are %v, want task default's step %d started once", l.key, starts, place)
		}
	}
	l.note("%s holds issue_plans %v, task default done, plan_steps %v, and one start of each step", l.key, versions, places)
	return nil
}

// refused is control-refused: a plan whose task a names `implementer checker` is refused 409
// PLAN_REFUSED with the refusal of a role nobody defines, naming task a, and records nothing.
func (l *ownerStepsLive) refused() {
	t := l.t
	t.Helper()
	plan := l.document("checker-plan.md", planOf("implementer", "checker"))
	status, body := l.registerPlan(plan, 1)
	var refusal api.PlanRefusal
	if status == http.StatusConflict {
		decodeInto(t, "the refusal of register_plan", body, &refusal)
	}
	want := api.PlanReason{Task: "a", Reason: "step 2 of task a names the role checker, which neither the deployment nor the plan defines."}
	if status != http.StatusConflict || refusal.Code != "PLAN_REFUSED" || !slices.Contains(refusal.Reasons, want) {
		t.Fatalf("register_plan of the plan `implementer checker` = %d %s, want 409 PLAN_REFUSED with the reason %+v", status, body, want)
	}
	if rows := l.planRows(); rows != 0 {
		t.Fatalf("the refused plan left %d plan rows on %s, want none", rows, l.key)
	}
	l.note("register_plan of `implementer checker`: %d %s; %s holds no plan rows", status, strings.TrimSpace(body), l.key)
}

// noPlanRows is control-no-plan-rows: the issue's owner gives the plan ownerStepsPlan, and its rows
// in plan_steps, plan_tasks and issue_plans, at every generation, are deleted from the daemon's
// store before its gate opens. It starts a planner, and the check of walk-implementer-reviewer must
// say so.
func (l *ownerStepsLive) noPlanRows() {
	t := l.t
	t.Helper()
	l.givePlan(ownerStepsPlan...)
	l.deletePlanRows()
	l.note("gave %s the plan %v and deleted its rows from plan_steps, plan_tasks and issue_plans before its gate opened", l.key, ownerStepsPlan)
	err := l.ownersPlanWalked(ownerStepsPlan)
	if err == nil || !strings.Contains(err.Error(), "first step is planning") {
		t.Fatalf("control: the check of the walk of %s, whose plan rows were deleted, says %v, want it to name planning as its first step", l.key, err)
	}
	l.awaitLaunch(l.token(claim.RolePlanner))
	l.note("control: %s started a planner, and the walk's check fails: %v", l.key, err)
}

// decodeInto decodes body into into, failing the test with what it decoded.
func decodeInto(t *testing.T, what, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decode %s %q: %v", what, body, err)
	}
}
