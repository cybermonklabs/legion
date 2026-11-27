package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// listingModels is returnEngine under a deployment that lists models, and defines no prober: the
// settings the daemon runs with, which a restart can change.
func listingModels(models ...string) *Engine {
	engine := returnEngine()
	engine.cfg.Deployment.Models = models
	return engine
}

// planProbedOnModel is the owner's plan of root, taken by engine: task a of steps, whose role
// prober the plan defines on model.
func planProbedOnModel(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue, model string, steps ...string) {
	t.Helper()
	given := ownersPlan(t, root.Key, root.Generation, 1, givenTask(t, "Build the widget, and probe it.", steps...))
	given.Plan.Roles = []plan.PlanRole{{Name: "prober", Type: plan.TypeTesting, Model: model, Prompt: "# Prober\n\nProbe the change."}}
	mustTake(t, "the plan that defines prober on "+model, applyFacts(t, pool, engine)(planID(root.Key, root.Generation, 1), given))
}

// proberAtStep2 is an issue whose plan, taken while the deployment listed m-a and m-b, defines
// prober on model, at step 2 of `implementer prober`, the implementer's step passed and the
// prober's running.
func proberAtStep2(t *testing.T, pool *pgxpool.Pool, model string) record.Issue {
	t.Helper()
	before := listingModels("m-a", "m-b")
	root := stepRoot(t, pool, phase.Admitted)
	planProbedOnModel(t, pool, before, root, model, "implementer", "prober")
	seedTreeLifecycle(t, pool, root.Key)
	move(t, pool, before, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	pass := intake.HandoffComplete{Generation: root.Generation, Issue: root.Key, Role: claim.RoleImplementer,
		Claim: claim.Token(treeRoleToken(t, root.Key, claim.RoleImplementer)), Summary: "built", Commit: stepCommitW, HandoffRecord: "impl-1", HandoffVersion: 1}
	if result := applyFacts(t, pool, before)("impl-1", pass); result.Refusal != nil {
		t.Fatalf("the implementer's pass was refused: %+v", result.Refusal)
	}
	if task := taskOf(t, pool, before, root.Key); task.State != record.TaskRunning || task.Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 2}) {
		t.Fatalf("task a after the implementer's pass = %s at %+v (%q), want running at the prober's step", task.State, task.Place, task.StoppedReason)
	}
	return root
}

// modelsTypoFile is a legion.yaml whose `models` misses a comma: `[m-a m-b]` is one entry, which
// holds white space, where `[m-a, m-b]` was meant.
const modelsTypoFile = `project: DEMO
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
models: [m-a m-b]
`

// A typo in `models` refuses the whole file at load, naming the entry, so no daemon boots on it
// and the store keeps every task as it stood. A loader that took the file, leaving the entry out,
// would list no model, and the boot on it would stop each task whose plan role runs on m-a or
// m-b; fixing the file would start none of them again. The release before refuses the key
// `models` itself, so no file an earlier release took holds one, and no warning release is owed.
func TestATypoInModelsRefusesTheLoadAndStopsNoTask(t *testing.T) {
	pool := migratedPool(t)
	root := proberAtStep2(t, pool, "m-a")
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(modelsTypoFile), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path, func(string) string { return "" })
	if err == nil {
		// The daemon boots on the settings it loaded.
		booted := returnEngine()
		booted.cfg.Deployment = cfg.Deployment()
		applyRefusingNothing(t, pool, booted, intake.DaemonBooted{Boot: 1})
	}

	const refusal = `models[0] is "m-a m-b", which holds white space: a model's name is one word`
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Errorf("Load of models: [m-a m-b] = %v (listed %v, warnings %q), want a refusal naming %q", err, cfg.ListedModels(), cfg.Warnings, refusal)
	}
	if task := taskOf(t, pool, listingModels("m-a", "m-b"), root.Key); task.State != record.TaskRunning {
		t.Fatalf("task a after the load of models: [m-a m-b] = %s (%q), want running: a refused file boots no daemon", task.State, task.StoppedReason)
	}
}

// A daemon that restarts with settings whose `models` no longer lists the model of a role the
// plan in force defines, while a step of that role is still to run, stops that task at its boot as
// it stops one whose role the deployment no longer defines: the prober running at step 2 on m-b
// is stopped (step_stop), the task stops with the reason that names the step, the role and the
// model, and the owner is told once, in a task-stopped notice that names the prober. A restart
// whose settings still list m-b stops nothing.
func TestABootStopsATaskWhosePlanRolesModelTheDeploymentNoLongerLists(t *testing.T) {
	pool := migratedPool(t)
	root := proberAtStep2(t, pool, "m-b")
	after := listingModels("m-a")
	applyRefusingNothing(t, pool, after, intake.DaemonBooted{Boot: 1})

	const why = "step 2 names the role prober, whose model m-b the deployment no longer lists; it lists m-a"
	if task := taskOf(t, pool, after, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
		t.Fatalf("task a after the boot = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
	}
	if stops := superviseRequests(t, pool, "step_stop"); stops[root.Key+"/prober"] != 1 {
		t.Fatalf("step_stop rows %v, want one for the running prober", stops)
	}
	stopped := noticesOf(t, pool, root.Key, "task-stopped")
	if len(stopped) != 1 || stopped[0].Role != "prober" || stopped[0].Reason != "task a of "+root.Key+" was stopped: "+why {
		t.Fatalf("the task-stopped notices = %+v, want exactly one, the prober's, with the reason", stopped)
	}

	kept := migratedPool(t)
	listed := proberAtStep2(t, kept, "m-b")
	applyRefusingNothing(t, kept, listingModels("m-b"), intake.DaemonBooted{Boot: 1})
	if task := taskOf(t, kept, after, listed.Key); task.State != record.TaskRunning {
		t.Fatalf("task a under settings that list m-b = %s (%q) after the boot, want running", task.State, task.StoppedReason)
	}
	if stops := superviseRequests(t, kept, "step_stop"); len(stops) != 0 {
		t.Fatalf("step_stop rows under settings that list m-b = %v, want none", stops)
	}
}

// The move into a step of a role the plan in force defines on a model the deployment no longer
// lists starts no role and stops the task, as a move into the step of a role the deployment lost
// does (lostAt): a root that waited on a person before its first step at the boot is stopped
// there, so the gate's opening into `prober implementer` starts no prober on m-b, the task stops
// naming step 1, the role and the model, and the owner is told once.
func TestTheMoveIntoAStepWhosePlanRolesModelIsUnlistedStopsTheTask(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Admitted)
	planProbedOnModel(t, pool, listingModels("m-a", "m-b"), root, "m-b", "prober", "implementer")
	seedTreeLifecycle(t, pool, root.Key)
	after := listingModels("m-a")

	move(t, pool, after, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})

	if starts := roleStartsOf(t, pool, root.Key, "prober"); len(starts) != 0 {
		t.Fatalf("the gate's opening started the prober %+v, want none: the deployment no longer lists its model", starts)
	}
	const why = "step 1 names the role prober, whose model m-b the deployment no longer lists; it lists m-a"
	if task := taskOf(t, pool, after, root.Key); task.State != record.TaskStopped || task.StoppedReason != why {
		t.Fatalf("task a = %s stopped with %q, want stopped with %q", task.State, task.StoppedReason, why)
	}
	stopped := noticesOf(t, pool, root.Key, "task-stopped")
	if len(stopped) != 1 || stopped[0].Role != "prober" || stopped[0].Reason != "task a of "+root.Key+" was stopped: "+why {
		t.Fatalf("the task-stopped notices = %+v, want exactly one, the prober's, with the reason", stopped)
	}
}
