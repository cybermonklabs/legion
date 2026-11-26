//go:build e2e

// The live proof's driver of a role an owner's plan defines: the owner of an issue writes plan.md
// with a role block, prober of the type testing on the model prober-model, through the real
// `dispatch` CLI and gives it to Legion with register_plan, and the whole daemon runs the
// implementer, then the prober with the plan's prompt on the plan's model, then the retro and the
// merge. scripts/e2e/plan-roles.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against.
//
// Nothing of a role's path is stood in for. Dispatch is the real server this checkout builds
// (startHandoffDispatch), on a database of its own on LEGION_TEST_PG_DSN's server, so the role block
// of each version is read by Dispatch's own parser. The daemon is the whole daemon (run, through
// startDaemonWithin) with dispatch_url at that server, no role of the deployment and the listed
// models [prober-model], so register_plan reads the plan's version and its blocks from Dispatch with
// the daemon's own client and judges the role's model against that list, and each step record goes
// through the daemon's outbox to the real route. The issues walk as the package's whole-daemon test
// of a role a plan defines walks them (walkImplementerProber, plan_roles_test.go). The owner writes
// with the real CLI under bun in its own session, and the reader of the steps is the same CLI as a
// person of the project, with the personal token Dispatch minted for that person
// (stepRecordsLive.cli).
//
// What is stood in for lies outside the role: GitHub's REST API (stepRecordsGitHub), the Apps'
// tokens, Envoy's listener, and the panes, which the package's fake runtime launches and the driver
// plays, each answering with the model its launch's spec named.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// planRolesLive is one run of the proof: the rig over the run's issues and the person's CLI over the
// same rig.
type planRolesLive struct {
	*deploymentRolesRig
	reads *stepRecordsLive
}

// TestPlanRolesLive drives the proof's checks against one booted daemon and one Dispatch, over three
// issues of one project: the first walks its owner's plan, which defines prober and names
// `implementer prober`; the second, the control, is given the same task with no role block; the
// third is given prober on the model other-model, which the deployment does not list.
func TestPlanRolesLive(t *testing.T) {
	fixtureenv.Live(t)
	proof := newLiveProof(t, "scripts/e2e/plan-roles.sh")
	proof.cfg.Models = []string{planProberModel}
	root := liveCLICheckout(t)
	rig := newDeploymentRolesRig(t, proof, startHandoffDispatch(t, root).scratchDispatch, nil,
		[]string{"Prober probes the change", "No role block is refused", "Unlisted model is refused"})
	l := &planRolesLive{deploymentRolesRig: rig, reads: &stepRecordsLive{stepRecordsRig: rig.stepRecordsRig, root: root, cliHome: t.TempDir()}}
	l.reads.mintReader("plan-roles-reader")
	runs, noBlock, otherModel := l.issues[0].key, l.issues[1].key, l.issues[2].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s and %s; daemon at dispatch_url %s with no role of the deployment and the listed models %v",
		l.dispatch.base, l.cfg.Project, liveRepo, runs, noBlock, otherModel, l.cfg.DispatchURL, l.cfg.ListedModels())

	l.on(runs)
	var plan string
	l.check("role-block-stored", func() { plan = l.roleBlockStored() })
	l.check("plan-role-runs", func() { l.planRoleRuns(plan) })
	l.check("control-no-role-block", func() {
		l.on(noBlock)
		l.refused(planWithProber("", false), "step 2 of task a names the role prober, which neither the deployment nor the plan defines.")
	})
	l.check("model-not-listed", func() {
		l.on(otherModel)
		l.refused(planWithProber("other-model", true), "step 2 of task a names the role prober, whose model other-model the deployment does not list; it lists prober-model.")
	})
}

// roleBlockStored is the owner's session, the architect's claim of the walk's issue, writing plan.md
// on the issue (planWithProber: the role block prober of the type testing on prober-model, then task
// a `implementer prober`) through the real `dispatch artifact` command, which prints the document's
// reference `dispatch://<KEY>/artifact/plan-md`, and the daemon's own client reading it back at
// version 1 with its blocks (GET /api/v1/artifacts/{id}/versions/1?blocks=true): the block
// plan_role prober with type=testing, model=prober-model and the prompt as its body, and the block
// plan_task a. The document's id, which register_plan takes, is the one the issue's own read of its
// documents names for that slug (GET /api/v1/issues/{key}/summary). It answers that id.
func (l *planRolesLive) roleBlockStored() string {
	t := l.t
	t.Helper()
	owner := l.worker(claim.RoleArchitect)
	shell := cliShell{t: t, dir: l.reads.cliHome, session: owner.session, base: l.dispatch.base, token: l.dispatch.token}
	out, code := shell.bun(nil, dispatchCLI(l.reads.root), "artifact", "--issue", l.key, "--name", "plan.md", "--content", planWithProber(planProberModel, true))
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
		t.Fatalf("read %s at version 1 with its blocks as the daemon: %v", id, err)
	}
	var role, task *dispatch.DocumentBlock
	for i, block := range read.Blocks {
		switch {
		case block.Type == "plan_role" && block.ID == string(planProber):
			role = &read.Blocks[i]
		case block.Type == "plan_task" && block.ID == "a":
			task = &read.Blocks[i]
		}
	}
	switch {
	case role == nil || task == nil || len(read.Blocks) != 2 || read.IssueKey != l.key:
		t.Fatalf("%s at version 1 reads %+v, want the blocks plan_role prober and plan_task a on %s", id, read, l.key)
	case role.Attributes["type"] != "testing" || role.Attributes["model"] != planProberModel || strings.TrimSpace(role.Text) != planProberPrompt:
		t.Fatalf("the block plan_role prober of %s reads the attributes %v and the body %q, want type=testing, model=%s and %q", id, role.Attributes, role.Text, planProberModel, planProberPrompt)
	}
	l.note("the owner's session %s wrote plan.md (%s) on %s with `dispatch artifact`; version 1 read with ?blocks=true lists plan_role prober (type=%s, model=%s, body %q) and plan_task a (steps %q)",
		owner.session, id, l.key, role.Attributes["type"], role.Attributes["model"], strings.TrimSpace(role.Text), task.Attributes["steps"])
	return id
}

// planRoleRuns is the owner giving version 1 of plan, before the gate opens, with POST
// /legion/v1/plans/register under the architect's grant: 200, the plan at version 1 with one task a
// naming implementer and prober. The issue then walks `implementer prober` (walkImplementerProber),
// and `dispatch steps --issue <KEY>`, run as the person, prints step 1 implementer of the type
// implementation, then step 2 prober of the type testing with the model prober-model, then the
// retro and the merge, each ended passed, and no other run.
func (l *planRolesLive) planRoleRuns(plan string) {
	t := l.t
	t.Helper()
	status, body := l.registerPlan(plan, 1)
	if status != http.StatusOK {
		t.Fatalf("register_plan of %s version 1 = %d %s, want 200", plan, status, body)
	}
	var taken api.PlanRegisterResponse
	decodeInto(t, "the answer of register_plan", body, &taken)
	if taken.Plan.Version != 1 || len(taken.Plan.Tasks) != 1 || taken.Plan.Tasks[0].ID != "a" || !slices.Equal(taken.Plan.Tasks[0].Steps, []string{"implementer", string(planProber)}) {
		t.Fatalf("register_plan answered %s, want version 1 with one task a naming implementer and prober", body)
	}
	l.note("POST /legion/v1/plans/register with the architect's grant, version 1 of %s on %s: 200 %s", plan, l.key, strings.TrimSpace(body))
	if err := l.walkImplementerProber(); err != nil {
		t.Fatal(err)
	}
	l.note("%s walked implementing, step (prober, its spawn spec naming %s and its prompt holding the plan's), retro and merging to done", l.key, planProberModel)
	var out string
	var err error
	// The records ride the outbox, so `dispatch steps` holds them a moment after the walk ends.
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		out, code = l.reads.steps()
		if code != 0 {
			t.Fatalf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
		}
		if err = planRoleLinesHold(l.key, out); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("`dispatch steps --issue %s` printed\n%s\nand the read does not hold the walk of `implementer prober`: %v", l.key, out, err)
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
}

// planRoleLinesHold says whether out, what `dispatch steps --issue <issue>` printed, is exactly the
// four runs of task a under a header of one task: step 1 implementer of the type implementation
// with the model stand-in/model, step 2 prober of the type testing with the model prober-model,
// the retro and the merge, each attempt 1, with an agent, ended passed. The error names the first
// run that is missing or wrong.
func planRoleLinesHold(issue, out string) error {
	printed, err := parseSteps(issue, out)
	if err != nil {
		return err
	}
	models := map[int]string{1: stepRecordsModel, 2: planProberModel}
	want := []stepWant{
		{kind: "step", step: 1, attempt: 1, role: "implementer", typ: "implementation", result: "passed"},
		{kind: "step", step: 2, attempt: 1, role: string(planProber), typ: "testing", result: "passed"},
		{kind: "retro", attempt: 1, role: "implementer", result: "passed"},
		{kind: "merge", attempt: 1, role: "merger", result: "passed"},
	}
	for _, w := range want {
		got, name := findLine(printed.lines, w), lineName(w)
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "a" || got.role != w.role || got.typ != printedType(w) || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task a, role %s, type %s and an agent", name, got.text, w.role, printedType(w))
		case !printedMoment(got.ended) || got.result != w.result:
			return fmt.Errorf("%s prints %q: want an end and result %s", name, got.text, w.result)
		case w.kind == "step" && got.model != models[w.step]:
			return fmt.Errorf("%s prints %q: want the model %s", name, got.text, models[w.step])
		}
	}
	if len(printed.lines) != len(want) || printed.tasks != 1 {
		return fmt.Errorf("the header says %d steps in %d tasks and %d lines follow it, want %d steps in 1 tasks", printed.steps, printed.tasks, len(printed.lines), len(want))
	}
	return nil
}

// refused is the owner giving version 1 of a plan.md whose text is text with register_plan: 409
// PLAN_REFUSED with the reason reason for task a, and no plan rows on the walk's issue.
func (l *planRolesLive) refused(text, reason string) {
	t := l.t
	t.Helper()
	plan := l.document("plan.md", text)
	status, body := l.registerPlan(plan, 1)
	var refusal api.PlanRefusal
	if status == http.StatusConflict {
		decodeInto(t, "the refusal of register_plan", body, &refusal)
	}
	want := api.PlanReason{Task: "a", Reason: reason}
	if status != http.StatusConflict || refusal.Code != "PLAN_REFUSED" || !slices.Contains(refusal.Reasons, want) {
		t.Fatalf("register_plan of %s on %s = %d %s, want 409 PLAN_REFUSED with the reason %+v", plan, l.key, status, body, want)
	}
	var rows int
	if err := l.st.Pool().QueryRow(context.Background(), `select (select count(*) from issue_plans where issue = $1)
		+ (select count(*) from plan_roles where issue = $1)`, l.key).Scan(&rows); err != nil {
		t.Fatalf("read the plan rows of %s: %v", l.key, err)
	}
	if rows != 0 {
		t.Fatalf("the refused plan left %d rows in issue_plans and plan_roles on %s, want none", rows, l.key)
	}
	l.note("register_plan of version 1 of %s on %s: %d %s; %s holds no plan rows", plan, l.key, status, strings.TrimSpace(body), l.key)
}
