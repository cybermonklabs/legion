// The whole daemon runs a role an owner's plan defines with a role block: the plan's prompt, the
// plan's model and a step record of the role's type. An issue that ran before a plan could define a
// role goes on.
//
// The first test boots the whole daemon over the rig of the deployment's roles
// (newDeploymentRolesRig, deployment_roles_test.go), whose settings here define no role and list
// the model prober-model: the real store, the real outbox and the real Dispatch of this package's
// tests, each role's agent over the worker stream, the owner's plan given with register_plan.
// GitHub's REST API and the panes are stood in for, each pane answering with the model its launch's
// spec named. The second boots it over the rig of an earlier release's store
// (newEarlierReleaseRig, deployment_roles_test.go), stopped below the migration of the roles a plan
// defines.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

const (
	// planProber is the role the owner's plan defines, of the type testing, with planProberModel,
	// the one model the deployment lists, and planProberPrompt, the block's text.
	planProber       claim.Role = "prober"
	planProberModel             = "prober-model"
	planProberPrompt            = "# Prober\n\nProbe the change."
)

// planRolesMigration ends the name of the migration of the roles a plan defines (plan_roles). It is
// found by its name rather than its number, as deploymentRolesMigration is.
const planRolesMigration = "_plan_roles.up.sql"

// planWithProber is the text of a plan.md whose task a names `implementer prober`
// (deploymentPlanOf), after a role block of prober of the type testing on model, or with no role
// block where block is false.
func planWithProber(model string, block bool) string {
	text := deploymentPlanOf("implementer", string(planProber))
	if !block {
		return text
	}
	return ":::plan_role{#prober type=\"testing\" model=\"" + model + "\"}\n" + planProberPrompt + "\n:::\n\n" + text
}

// The deployment lists prober-model; the owner's plan defines prober (type testing,
// that model, a prompt) and task a with `implementer prober`. The prober starts after the
// implementer's step passed, with the plan's prompt between Legion's parts and the plan's model; its
// task holds the task block's text and the implementer's report; the read of the issue's steps
// names step 2 prober, type testing, model prober-model, the model that answered. A plan that gives
// prober another model is refused, naming the model and the list.
//
// Control (the check's own): the plan without its role block is refused, naming prober, so no plan
// is in force and the issue's gate opens at planning, which the walk's assertion fails on.
func TestARoleThePlanDefinesRunsWithItsPromptAndModel(t *testing.T) {
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.Models = []string{planProberModel}
	r := newDeploymentRolesRig(t, proof, startScratchDispatchOn(t, scratchDispatchOptions{}), nil,
		[]string{"Prober probes the change", "Other model is refused", "No role block brings planner"})
	runs, otherModel, noBlock := r.issues[0].key, r.issues[1].key, r.issues[2].key

	r.on(runs)
	if status, body := r.registerPlan(r.document("plan.md", planWithProber(planProberModel, true)), 1); status != http.StatusOK {
		t.Fatalf("register_plan of %s's plan with the role block prober = %d %s, want 200", runs, status, body)
	}
	if err := r.walkImplementerProber(); err != nil {
		t.Fatal(err)
	}
	waitDeploymentSteps(r.stepRecordsRig, "a", true, []deploymentStepWant{
		{kind: "step", step: 1, role: "implementer", typ: "implementation", model: stepRecordsModel},
		{kind: "step", step: 2, role: string(planProber), typ: "testing", model: planProberModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})

	r.on(otherModel)
	status, body := r.registerPlan(r.document("plan.md", planWithProber("other-model", true)), 1)
	want := "step 2 of task a names the role prober, whose model other-model the deployment does not list; it lists prober-model."
	if status != http.StatusConflict || !strings.Contains(body, "PLAN_REFUSED") || !strings.Contains(body, want) {
		t.Fatalf("register_plan of prober on other-model = %d %s, want 409 PLAN_REFUSED saying %q", status, body, want)
	}

	t.Run("control: the plan without its role block", func(t *testing.T) {
		r.on(noBlock)
		status, body := r.registerPlan(r.document("plan.md", planWithProber("", false)), 1)
		want := "step 2 of task a names the role prober, which neither the deployment nor the plan defines."
		if status != http.StatusConflict || !strings.Contains(body, "PLAN_REFUSED") || !strings.Contains(body, want) {
			t.Fatalf("register_plan of `implementer prober` with no role block = %d %s, want 409 PLAN_REFUSED saying %q", status, body, want)
		}
		err := r.walkImplementerProber()
		if want := noBlock + "'s gate opened and its first step is planning"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("the walk's assertion on %s, whose plan was refused, says %v; want it to say %q", noBlock, err, want)
		}
	})
}

// A deployment that lists no `models` and whose role checker names checker-model lists that one
// model (config.Config.ListedModels). The whole daemon tells the owner checker-model as the model a
// role of its plan can name, takes a plan whose own role critic names it, and refuses a plan whose
// critic names another model, with checker-model as the list. `legion config check` and the served
// settings print the same list (TestCheckConfigPrintsTheListedModels,
// TestSettingsOfServesTheListedModels).
func TestARoleOfTheDeploymentListsItsModel(t *testing.T) {
	r := newDeploymentRolesRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), deploymentRoles(t),
		[]string{"Critic on the role's model", "Critic on another model"})
	listed, other := r.issues[0].key, r.issues[1].key
	critic := func(model string) string {
		return ":::plan_role{#critic type=\"testing\" model=\"" + model + "\"}\n# Critic\n\nCriticise the change.\n:::\n\n" +
			deploymentPlanOf("implementer", "critic")
	}

	r.on(listed)
	addressing := lastLaunch(t, r.rt, r.token(claim.RoleArchitect)).Prompt.Addressing
	if want := ModelsFragment([]string{deploymentCheckerModel}); !strings.Contains(addressing, want) {
		t.Errorf("the architect of %s is told %q, want it to hold %q", listed, addressing, want)
	}
	if status, body := r.registerPlan(r.document("plan.md", critic(deploymentCheckerModel)), 1); status != http.StatusOK {
		t.Errorf("register_plan of critic on %s, the model of the role checker = %d %s, want 200", deploymentCheckerModel, status, body)
	}

	r.on(other)
	status, body := r.registerPlan(r.document("plan.md", critic("other-model")), 1)
	want := "step 2 of task a names the role critic, whose model other-model the deployment does not list; it lists checker-model."
	if status != http.StatusConflict || !strings.Contains(body, "PLAN_REFUSED") || !strings.Contains(body, want) {
		t.Errorf("register_plan of critic on other-model = %d %s, want 409 PLAN_REFUSED saying %q", status, body, want)
	}
}

// The plan in force of issue A defines prober, and A's implementer is at work, when the operator
// adds a role prober to the deployment and restarts: the deployment's prober has a prompt of its
// own, the model deployment-prober-model and the implement App. A's plan keeps its definition for
// A: once A's implementer passes, A's prober launches with the plan's prompt, the plan's model and
// the review App of the plan's type testing, and its spawn spec says it is the plan's role, so the
// runtime gives it none of the deployment role's own settings. Issue B, whose plan defines no
// role, takes the deployment's prober: its prompt, its model and the implement App, and a spawn
// spec that says it is no role of a plan. The boot logs one line naming prober and A, and not B.
//
// The control is the order this replaces, run by hand: the deployment's role read before the
// plan's for every issue (specs.rolePromptPaths, appauth.AppRoleFor). A's prober then launches on
// the deployment's model, and the check of A's launch fails.
func TestARoleOfAPlanInForceKeepsItsDefinitionWhenTheDeploymentTakesItsName(t *testing.T) {
	const deploymentProberModel = "deployment-prober-model"
	const deploymentProberPrompt = "You are the deployment's prober: probe the deployed service and report pass or fail."
	proof := newLiveProofOn(t, workflowNATS(t))
	proof.cfg.Models = []string{planProberModel}
	r := newDeploymentRolesRig(t, proof, startScratchDispatchOn(t, scratchDispatchOptions{}), nil,
		[]string{"Plan's prober keeps its definition", "Deployment's prober serves another issue"})
	kept, other := r.issues[0].key, r.issues[1].key

	r.on(kept)
	if status, body := r.registerPlan(r.document("plan.md", planWithProber(planProberModel, true)), 1); status != http.StatusOK {
		t.Fatalf("register_plan of %s's plan with the role block prober = %d %s, want 200", kept, status, body)
	}
	if first := r.openGate(); first != phase.Implementing {
		t.Fatalf("%s's gate opened at %s, want implementing, the first step of its plan", kept, first)
	}
	implementer := r.worker(claim.RoleImplementer)
	implementer.take()

	prompt := filepath.Join(t.TempDir(), "prober.md")
	if err := os.WriteFile(prompt, []byte(deploymentProberPrompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(t.TempDir(), "daemon.log")
	t.Setenv(liveDaemonLogEnv, logs)
	r.restartWith([]config.Role{{Name: planProber, Type: "testing", PromptFile: prompt, Model: deploymentProberModel, GitHubApp: config.RoleAppImplement}},
		kept, other)
	// A restarted daemon knows no model until the agent answers again (supervise.StreamAnswered).
	implementer.ownTurn()
	r.finish(implementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
	if err := r.proberLaunched(planProberModel, planProberPrompt, "legion-review[bot]", true); err != nil {
		t.Fatalf("%s, whose plan in force defines prober: %v", kept, err)
	}

	r.on(other)
	r.givePlan("implementer", string(planProber))
	if first := r.openGate(); first != phase.Implementing {
		t.Fatalf("%s's gate opened at %s, want implementing, the first step of its plan", other, first)
	}
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
	if err := r.proberLaunched(deploymentProberModel, deploymentProberPrompt, "legion-implement[bot]", false); err != nil {
		t.Fatalf("%s, whose plan defines no role: %v", other, err)
	}

	written, err := os.ReadFile(logs)
	if err != nil {
		t.Fatalf("read the daemon's log: %v", err)
	}
	var lines []string
	for line := range strings.Lines(string(written)) {
		if strings.Contains(line, "has the name of a role the plan in force") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "role=prober") || !strings.Contains(lines[0], kept) || strings.Contains(lines[0], other) {
		t.Fatalf("the boot logged %q, want one line naming the role prober and %s alone", lines, kept)
	}
}

// proberLaunched says whether the latest launch of prober on the walk's issue names model, holds
// prompt in its launch prompt, commits as bot, the bot of the App it acts as, and says whether it
// is the role the issue's plan defines (planRole). The error names the first of them that does
// not hold.
func (r *deploymentRolesRig) proberLaunched(model, prompt, bot string, planRole bool) error {
	r.t.Helper()
	spec := lastLaunch(r.t, r.rt, r.worker(planProber).token)
	if spec.PlanRole != planRole {
		return fmt.Errorf("the prober's spawn spec says it is the plan's role: %t, want %t", spec.PlanRole, planRole)
	}
	if spec.Model != model {
		return fmt.Errorf("the prober's spawn spec names the model %q, want %s", spec.Model, model)
	}
	composed, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		return fmt.Errorf("compose the prober's launch prompt: %w", err)
	}
	if !strings.Contains(composed, prompt) {
		return fmt.Errorf("the prober's launch prompt lacks %q:\n%s", prompt, composed)
	}
	if got := spec.Env["GIT_AUTHOR_NAME"]; got != bot {
		return fmt.Errorf("the prober commits as %q, want %s", got, bot)
	}
	return nil
}

// An issue that ran before a plan could define a role goes on under this release. The store holds
// every migration below plan_roles, applied by hand (storeBelow), and the record of one issue
// whose owner's plan `implementer tester reviewer` stands at step 2, the implementer's step done
// and the tester's claim a launch the earlier daemon began (seedIssueAtTesting). This release's
// migrations then run on it, plan_roles among them, as a deploy runs them, and the whole daemon
// boots on it. The tester's pass moves task a to step 3, the reviewer starts with the task of step 3
// of 3, and plan_roles holds no row of the issue: the plan of the release before stays a plan that
// defines no role.
//
// The fault: a daemon that needs a plan_roles row of the plan in force could not read the issue's
// plan, since that plan has none, and the issue would not go on. The control is a mutation of the
// release's code, run by hand: record.Postgres.Plan failing where the generation's plan has no
// plan_roles row; the daemon's boot then fails at its read of the issue's plan, and the test fails
// before the tester's pass.
func TestAnIssueThatRanBeforePlanRolesGoesOn(t *testing.T) {
	r := newEarlierReleaseRig(t, planRolesMigration)
	r.work(claim.RoleTester, "test", testCompletion, phase.Reviewing)
	if kind, position := r.taskPlace(); kind != "step" || position != 3 {
		t.Fatalf("after the tester's pass %s's task a stands at %s %d, want step 3", r.key, kind, position)
	}
	reviewer := r.worker(claim.RoleReviewer)
	if task := reviewer.take().Message; !strings.Contains(task, " Task: a. Step 3 of 3: reviewer.") {
		t.Fatalf("the reviewer's task is %q, want it to stand at step 3 of 3 of task a", task)
	}
	reviewer.endTurn()
	var roles int
	if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from plan_roles where issue = $1`, r.key).Scan(&roles); err != nil {
		t.Fatalf("read the plan_roles rows of %s: %v", r.key, err)
	}
	if roles != 0 {
		t.Fatalf("plan_roles holds %d rows of %s, an issue whose plan defines no role, want none", roles, r.key)
	}
}

// walkImplementerProber is the walk of `implementer prober` on the walk's issue: its gate opens at
// implementing, the implementer works its step, the prober launches with the plan's model and a
// prompt that holds the plan's prompt and Legion's worker part, its task names the issue, the
// task's text and the implementer's report, and its pass runs the retro and the merge to done. The
// error names the first thing that does not hold.
func (r *deploymentRolesRig) walkImplementerProber() error {
	r.t.Helper()
	if first := r.openGate(); first != phase.Implementing {
		return fmt.Errorf("%s's gate opened and its first step is %s, want implementing, the first step of its plan", r.key, first)
	}
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Step)
	prober := r.worker(planProber)
	spec := lastLaunch(r.t, r.rt, prober.token)
	if spec.Model != planProberModel {
		return fmt.Errorf("the prober's spawn spec names the model %q, want %s", spec.Model, planProberModel)
	}
	prompt, err := runtime.ComposePrompt(spec.Prompt)
	if err != nil {
		return fmt.Errorf("compose the prober's launch prompt: %w", err)
	}
	for _, want := range []string{planProberPrompt, "SEPARATE-POD"} {
		if !strings.Contains(prompt, want) {
			return fmt.Errorf("the prober's launch prompt lacks %q:\n%s", want, prompt)
		}
	}
	task := takeTask(prober)
	for _, want := range []string{r.key, deploymentTaskText, " Step 1 (implementer) reported:"} {
		if !strings.Contains(task, want) {
			return fmt.Errorf("the prober's task lacks %q:\n%s", want, task)
		}
	}
	r.finish(prober, "", api.HandoffCompleteRequest{Summary: "every probe holds", Commit: stepRecordsHead, Verdict: "pass"}, phase.Retro)
	r.retro()
	r.merge()
	return nil
}
