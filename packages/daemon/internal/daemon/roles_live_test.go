//go:build e2e

// The live proof's driver of the roles of a deployment: the settings file defines checker, a role
// of the type testing with a prompt file and a model, the owner of an issue gives Legion the plan
// `implementer checker`, and the whole daemon runs the implementer, then the checker, whose launch
// and step record name the model the settings give it, then the retro and the merge.
// scripts/e2e/roles.sh builds this driver with `-tags e2e` and owns the nats-server and the
// PostgreSQL it runs against.
//
// The daemon's path of a step is the real one, and the agent is played by the driver over its
// pane's connection on the package's fake runtime. The settings are a legion.yaml the driver writes and
// reads with config.Load, the loader the daemon's boot reads its file with, and the daemon boots on
// the roles that load answers. Dispatch is the real server this checkout builds (startHandoffDispatch),
// on a database of its own on LEGION_TEST_PG_DSN's server. The daemon is the whole daemon (run,
// through startDaemonWithin) with dispatch_url at that server, so register_plan reads the plan's
// version from Dispatch with the daemon's own client, and each step record goes through the daemon's
// outbox to the real route. The issues walk as the package's whole-daemon tests of a deployment's
// roles walk them (deploymentRolesRig, deployment_roles_test.go). The reader is the real `dispatch`
// CLI (packages/envoy-client/bin/dispatch.ts under bun), run as a person of the project with the
// personal token Dispatch minted for that person (stepRecordsLive.cli).
//
// What is stood in for lies outside the steps: GitHub's REST API (stepRecordsGitHub), the Apps'
// tokens (workflowTokenRecorder), Envoy's listener (noticeListener), and the panes, which the
// package's fake runtime launches and the driver plays, each answering with the model its launch's
// spec named. No agent runs, so what an architect is told is read from its launch spec as the fake
// runtime recorded it, composed as a live launch composes it (runtime.ComposePrompt).
package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// rolesLiveSettings is the legion.yaml the driver loads: the smallest file the loader takes, with
// the deployment's roles checker and prober, each beside its prompt file. The daemon boots on the
// roles its load answers; every other value of the daemon's settings is the run's own
// (workflowConfig).
const rolesLiveSettings = `project: DEMO
state_dir: /var/lib/legion
postgres_dsn: postgres://legion@127.0.0.1:5432/legion
dispatch_url: http://127.0.0.1:8080
dispatch_token_file: /var/run/legion/DISPATCH_TOKEN
projects:
  DEMO: {}
gates:
  design: root-issues
github_apps:
  implement:
    app_id: "1"
    private_key: implement-test-key
  review:
    app_id: "2"
    private_key: review-test-key
nats_urls: [nats://127.0.0.1:4222]
`

// rolesLive is one run of the proof: the rig over the run's issues, the person's CLI over the same
// rig, and the directory of the settings file.
type rolesLive struct {
	*deploymentRolesRig
	reads    *stepRecordsLive
	settings string
}

// TestRolesLive drives the proof's checks against one booted daemon and one Dispatch, over three
// issues of one project: the first walks its owner's plan `implementer checker`, the second
// `implementer prober`, whose role names no model, and the third is given `implementer checker`
// once the daemon has restarted with checker taken out of the settings.
func TestRolesLive(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (Live).
	fixtureenv.Live(t)
	proof := newLiveProof(t, "scripts/e2e/roles.sh")
	root := liveCLICheckout(t)
	l := &rolesLive{settings: t.TempDir()}
	var roles []config.Role
	proof.step("checker-defined", func() { roles = l.checkerDefined(t) })
	rig := newDeploymentRolesRig(t, proof, startHandoffDispatch(t, root).scratchDispatch, roles,
		[]string{"Checker verifies widget output", "Prober answers without model", "Refusal names absent role"})
	l.deploymentRolesRig = rig
	l.reads = &stepRecordsLive{stepRecordsRig: rig.stepRecordsRig, root: root, cliHome: t.TempDir()}
	l.reads.mintReader("roles-reader")
	checked, unmodelled, refused := l.issues[0].key, l.issues[1].key, l.issues[2].key
	l.note("Dispatch %s (database of its own), project %s mapped to %s, issues %s, %s and %s; daemon at dispatch_url %s with the roles %v",
		l.dispatch.base, l.cfg.Project, liveRepo, checked, unmodelled, refused, l.cfg.DispatchURL, roleNames(l.cfg.Roles))

	l.on(checked)
	l.step("plan-registered", l.planRegistered)
	l.step("walk-implementer-checker", func() {
		if err := l.walkImplementerChecker(); err != nil {
			t.Fatal(err)
		}
		l.note("%s walked implementing, step (checker on %s), retro and merging to done; no planner and no tester was launched", l.key, deploymentCheckerModel)
	})
	l.step("checker-model", func() { l.stepRead(2, "checker", "testing", deploymentCheckerModel) })
	l.step("owner-lists-checker", l.ownerListsChecker)
	l.step("retro-rated", l.retroRated)
	l.step("control-no-model", func() {
		l.on(unmodelled)
		l.givePlan("implementer", "prober")
		l.walkProber()
		l.stepRead(2, "prober", "testing", stepRecordsModel)
	})
	l.step("control-no-role", func() {
		l.restartWith(l.loadSettings(t, "prober").Roles, refused)
		l.on(refused)
		status, body := l.registerPlan(l.document("plan.md", deploymentPlanOf("implementer", "checker")), 1)
		if status != http.StatusConflict || !strings.Contains(body, "PLAN_REFUSED") || !strings.Contains(body, "checker") {
			t.Fatalf("control: register_plan of `implementer checker` with checker out of the settings = %d %s, want 409 PLAN_REFUSED naming checker", status, body)
		}
		l.note("control: the daemon restarted on the settings without checker (roles %v); register_plan of `implementer checker` on %s: %d %s",
			roleNames(l.cfg.Roles), refused, status, strings.TrimSpace(body))
	})
}

// checkerDefined writes the settings file with `roles: {checker: {type: testing, prompt_file:
// checker.md, model: checker-model}, prober: {type: testing, prompt_file: prober.md}}` and reads it
// in the driver's own process with config.Load, the loader the daemon's boot reads its file with:
// RoleOf answers checker with its type, its prompt file resolved beside the file, its model and the
// review App, the App of a role whose block names none. It answers the roles the load read, which
// the daemon boots on.
func (l *rolesLive) checkerDefined(t *testing.T) []config.Role {
	t.Helper()
	loaded := l.loadSettings(t, "checker", "prober")
	got, ok := loaded.RoleOf(deploymentChecker)
	want := config.Role{Name: deploymentChecker, Type: "testing", PromptFile: filepath.Join(l.settings, "checker.md"), Model: deploymentCheckerModel, GitHubApp: config.RoleAppReview}
	if !ok || got != want {
		t.Fatalf("the settings load in the driver's process answers RoleOf(checker) = %+v, %v; want %+v", got, ok, want)
	}
	// The rig, whose note this would be, is built on the roles this load answers.
	fmt.Printf("   the settings load in the driver's process (config.Load of %s): RoleOf(checker) = %+v\n", filepath.Join(l.settings, "legion.yaml"), got)
	return loaded.Roles
}

// loadSettings writes the settings file defining the roles named, of checker and prober, beside
// their prompt files, and answers what config.Load reads of it.
func (l *rolesLive) loadSettings(t *testing.T, named ...string) config.Config {
	t.Helper()
	blocks := map[string]string{
		"checker": "checker: {type: testing, prompt_file: checker.md, model: " + deploymentCheckerModel + "}",
		"prober":  "prober: {type: testing, prompt_file: prober.md}",
	}
	prompts := map[string]string{
		"checker": deploymentCheckerPrompt,
		"prober":  "You are the prober of this deployment: probe the change and report pass or fail.",
	}
	var defined []string
	for _, name := range named {
		defined = append(defined, blocks[name])
		if err := os.WriteFile(filepath.Join(l.settings, name+".md"), []byte(prompts[name]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(l.settings, "legion.yaml")
	if err := os.WriteFile(path, []byte(rolesLiveSettings+"roles: {"+strings.Join(defined, ", ")+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path, os.Getenv)
	if err != nil {
		t.Fatalf("the settings load in the driver's process: config.Load(%s): %v", path, err)
	}
	return loaded
}

// roleNames is the name of each role of roles, in order.
func roleNames(roles []config.Role) []claim.Role {
	names := make([]claim.Role, len(roles))
	for i, role := range roles {
		names[i] = role.Name
	}
	return names
}

// planRegistered is the owner giving Legion version 1 of plan.md, one task a whose steps are the
// implementer and then the checker, before the issue's gate opens, with POST
// /legion/v1/plans/register under the architect's grant: 200, the plan at version 1 with one task a
// naming implementer and checker.
func (l *rolesLive) planRegistered() {
	t := l.t
	t.Helper()
	plan := l.document("plan.md", deploymentPlanOf("implementer", "checker"))
	status, body := l.registerPlan(plan, 1)
	if status != http.StatusOK {
		t.Fatalf("register_plan of %s version 1 = %d %s, want 200", plan, status, body)
	}
	var taken api.PlanRegisterResponse
	if err := json.Unmarshal([]byte(body), &taken); err != nil {
		t.Fatalf("decode the answer of register_plan %s: %v", body, err)
	}
	if taken.Plan.Version != 1 || len(taken.Plan.Tasks) != 1 || taken.Plan.Tasks[0].ID != "a" || !slices.Equal(taken.Plan.Tasks[0].Steps, []string{"implementer", "checker"}) {
		t.Fatalf("register_plan answered %s, want version 1 with one task a naming implementer and checker", body)
	}
	l.note("POST /legion/v1/plans/register with the architect's grant, version 1 of plan.md (%s) on %s: 200 %s", plan, l.key, strings.TrimSpace(body))
}

// walkProber walks the plan `implementer prober` on the walk's issue: its gate opens at
// implementing, the implementer works its step, the prober launches with no model in its spawn spec
// and passes, and the retro and the merge walk it to done.
func (l *rolesLive) walkProber() {
	t := l.t
	t.Helper()
	if first := l.openGate(); first != "implementing" {
		t.Fatalf("%s, given `implementer prober`, opened its gate at %s, want implementing", l.key, first)
	}
	l.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, "step")
	prober := l.worker(deploymentProber)
	if model := lastLaunch(t, l.rt, prober.token).Model; model != "" {
		t.Fatalf("the prober's spawn spec names the model %q, want none: the settings give prober none", model)
	}
	prober.take()
	l.finish(prober, "", api.HandoffCompleteRequest{Summary: "every probe holds", Commit: stepRecordsHead, Verdict: "pass"}, "retro")
	l.retro()
	l.merge()
	l.note("%s walked implementing, step (prober, its spawn spec naming no model), retro and merging to done", l.key)
}

// stepRead runs `dispatch steps --issue <KEY>` as the person and holds that it prints step 2 of task
// a, of role and typ, attempt 1, with an agent, the model that answered and the result passed, then
// the retro and the merge, each ended passed.
func (l *rolesLive) stepRead(step int, role, typ, model string) {
	t := l.t
	t.Helper()
	var out string
	var err error
	// The records ride the outbox, so `dispatch steps` holds them a moment after the walk ends.
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		out, code = l.reads.steps()
		if code != 0 {
			t.Fatalf("`dispatch steps --issue %s` exited %d: %s", l.key, code, out)
		}
		if err = rolesLinesHold(l.key, out, step, role, typ, model); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("`dispatch steps --issue %s` printed\n%s\nand the read does not hold step %d %s: %v", l.key, out, step, role, err)
	}
	l.note("`dispatch steps --issue %s` as the person, exit 0:", l.key)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
}

// rolesLinesHold says whether out, what `dispatch steps --issue <issue>` printed, holds step of task
// a, of role and typ, attempt 1, with an agent, the model model and the result passed, and the retro
// and the merge of task a, each ended passed.
func rolesLinesHold(issue, out string, step int, role, typ, model string) error {
	printed, err := parseSteps(issue, out)
	if err != nil {
		return err
	}
	for _, w := range []stepWant{{kind: "step", step: step, attempt: 1, role: role, typ: typ, result: "passed"},
		{kind: "retro", attempt: 1, role: "implementer", result: "passed"}, {kind: "merge", attempt: 1, role: "merger", result: "passed"}} {
		got, name := findLine(printed.lines, w), lineName(w)
		wantType := w.typ
		if wantType == "" {
			wantType = "-"
		}
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "a" || got.role != w.role || got.typ != wantType || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task a, role %s, type %s and an agent", name, got.text, w.role, wantType)
		case got.ended == "running" || got.ended == "-" || got.result != w.result:
			return fmt.Errorf("%s prints %q: want an end and result %s", name, got.text, w.result)
		case w.kind == "step" && got.model != model:
			return fmt.Errorf("%s prints %q: want the model %s", name, got.text, model)
		}
	}
	return nil
}

// ownerListsChecker reads the launch spec of the walk's issue's architect as the fake runtime
// recorded it (fake.Runtime.CallsOf("Spawn")), composed as a live launch composes it
// (runtime.ComposePrompt): the text holds `checker (testing)`, the name and the type the
// deployment's settings give its role. The note prints the line that names it.
func (l *rolesLive) ownerListsChecker() {
	t := l.t
	t.Helper()
	architect := l.token(claim.RoleArchitect)
	var spawns []runtime.SpawnSpec
	for _, call := range l.rt.CallsOf("Spawn") {
		if call.Spec.Claim == architect {
			spawns = append(spawns, call.Spec)
		}
	}
	if len(spawns) == 0 {
		t.Fatalf("the fake runtime recorded no Spawn of %s", architect)
	}
	composed, err := runtime.ComposePrompt(spawns[len(spawns)-1].Prompt)
	if err != nil {
		t.Fatalf("compose the launch prompt of %s: %v", architect, err)
	}
	const want = "checker (testing)"
	at := strings.Index(composed, want)
	if at < 0 {
		t.Fatalf("the architect's launch spec of %s, composed, does not hold %q:\n%s", l.key, want, composed)
	}
	line, _, _ := strings.Cut(composed[strings.LastIndex(composed[:at], "\n")+1:], "\n")
	l.note("the architect's launch spec of %s (Spawn of %s), composed: %s", l.key, architect, line)
}

// retroRated holds the rating the retro of the walk's issue gave the plan: the record of its retro,
// as GET /api/v1/issues/{key}/steps answers the read `dispatch steps` makes, names rating 4 and the
// reason "The plan fit the work." (waitDeploymentSteps), and `dispatch plans --project <PROJECT>`
// run as the person prints the issue with the owner's plan of one task, `implementer checker`, and
// that one rating with its reason.
func (l *rolesLive) retroRated() {
	t := l.t
	t.Helper()
	waitDeploymentSteps(l.stepRecordsRig, "a", true, []deploymentStepWant{
		{kind: "step", step: 1, role: "implementer", typ: "implementation", model: stepRecordsModel},
		{kind: "step", step: 2, role: "checker", typ: "testing", model: deploymentCheckerModel},
		{kind: "retro", role: "implementer"},
		{kind: "merge", role: "merger"},
	})
	l.note("GET /api/v1/issues/%s/steps: the retro of task a names rating %d and the reason %q", l.key, deploymentRating, deploymentReason)
	var out string
	var err error
	// The close and the rating ride the outbox, so the read by project lists the issue a moment
	// after its walk ends.
	for deadline := time.Now().Add(stepRecordsWait); ; time.Sleep(time.Second) {
		var code int
		if out, code = l.reads.cli("plans", "--project", l.cfg.Project); code != 0 {
			t.Fatalf("`dispatch plans --project %s` exited %d: %s", l.cfg.Project, code, out)
		}
		if err = l.ratedLineHolds(out); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("`dispatch plans --project %s` printed\n%s\nand does not hold %s's rating: %v", l.cfg.Project, out, l.key, err)
	}
	l.note("`dispatch plans --project %s` as the person, exit 0:", l.cfg.Project)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		l.note("  %s", line)
	}
}

// ratedLineHolds says whether out, what `dispatch plans --project <PROJECT>` printed, lists the walk's
// issue with the owner's plan of one task, `implementer checker`, and the one rating 4/5 with its
// reason.
func (l *rolesLive) ratedLineHolds(out string) error {
	lines, err := parsePlans(l.cfg.Project, out)
	if err != nil {
		return err
	}
	at := slices.IndexFunc(lines, func(line planLine) bool { return line.issue == l.key })
	switch {
	case at < 0:
		return fmt.Errorf("no line of %s", l.key)
	case lines[at].plan != "owner (1 tasks: implementer checker)":
		return fmt.Errorf("%s prints plan %s, want owner (1 tasks: implementer checker)", l.key, lines[at].plan)
	case len(lines[at].ratings) != 1 || lines[at].ratings[0] != (printedRating{rating: deploymentRating, reason: deploymentReason}):
		return fmt.Errorf("%s prints the ratings %+v, want the one rating %d/5 %q", l.key, lines[at].ratings, deploymentRating, deploymentReason)
	}
	return nil
}
