package sandbox

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// baselineRoleFiles are the fixture's role_config_files: one shared overlay and one of the role's
// own, for each of the six workflow roles and the daemon-launched controller, every one of them a
// file the operator's creds Secret volume holds (operatorPod). Every role is named, and named with
// two files in a fixed order, so the baseline golden below catches a later change that drops a
// role's overlays or reorders them.
var baselineRoleFiles = map[claim.Role][]string{
	claim.RoleArchitect:   {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/architect.yml"},
	claim.RolePlanner:     {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/planner.yml"},
	claim.RoleImplementer: {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/implementer.yml"},
	claim.RoleTester:      {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/tester.yml"},
	claim.RoleReviewer:    {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/reviewer.yml"},
	claim.RoleMerger:      {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/merger.yml"},
	claim.RoleController:  {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/controller.yml"},
}

// baselineRoles are the roles the baseline covers: the six of the workflow, in the order the
// workflow reaches them, then the project controller, which is none of them.
func baselineRoles() []claim.Role {
	return slices.Concat(claim.Roles, []claim.Role{claim.RoleController})
}

// baselineRuntime is the test runtime of a deployment that declares no memory block at all —
// memoryRuntime's shape with no memory overlays — over the operator's pod, whose own
// PI_CONFIG_FILES entry every pod carries, with baselineRoleFiles as role_config_files.
func baselineRuntime(t *testing.T) *Runtime {
	t.Helper()
	return runtimeWithOptions(t, func(o *Options) {
		o.RoleConfigFiles = baselineRoleFiles
		o.MemoryConfigFiles = nil
	})
}

// baselineSpec is role's launch under that deployment: a role of the tree's root issue, or the
// project controller's own launch, which runs in no tree. Neither carries a memory group
// (SpawnSpec.Memory) and neither names memory instructions, so each is a launch of a deployment
// that declares nothing beyond the roles.
func baselineSpec(t *testing.T, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	if role == claim.RoleController {
		return controllerSpec(t)
	}
	token, err := claim.NewToken(testProject, testTree, role)
	if err != nil {
		t.Fatal(err)
	}
	return testSpec(t, token, role, testTree)
}

// launchBaseline is what the golden records for one role: the two values a launch hands the role's
// Oh My Pi.
type launchBaseline struct {
	PiConfigFiles      string `json:"PI_CONFIG_FILES"`
	AppendSystemPrompt string `json:"--append-system-prompt"`
}

// appendSystemPromptOf is the value of the one --append-system-prompt word of argv, the start
// command's agent argv (launcherCommand, agentArgv).
func appendSystemPromptOf(t *testing.T, role claim.Role, argv []string) string {
	t.Helper()
	i := slices.Index(argv, "--append-system-prompt")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("the %s's start command names no --append-system-prompt value: %q", role, argv)
	}
	return argv[i+1]
}

// The launch bytes a deployment that declares no experiment produces today, for every role Legion
// launches: the golden pins two values per role — the role's PI_CONFIG_FILES, the operator's own
// overlays first and then the role's own, and the value of the one --append-system-prompt word its
// Oh My Pi is started with. Both are kept non-empty for all seven roles by the fixture
// (operatorPod's PI_CONFIG_FILES entry and baselineRoleFiles), so a later change that drops an
// overlay, reorders the list, or moves a prompt fragment shows here as a diff to read.
//
// The two values are read off workerOf's container rather than off manifestOf's Sandbox because
// neither rides the pod manifest: the pod template carries the operator's PI_CONFIG_FILES entry
// alone and no prompt at all, since a role's own overlays and its prompt ride the launcher's start
// command (launcherCommand). workerOf is that whole process — the pod's container as the pod runs
// it, with the start command's argv and environment applied over it — which is what a rewrite of
// the launch path must leave unchanged for such a deployment.
func TestLaunchBaselineOfADeploymentWithNoExperiment(t *testing.T) {
	r := baselineRuntime(t)
	baseline := map[string]launchBaseline{}
	for _, role := range baselineRoles() {
		worker := workerOf(t, r, baselineSpec(t, role))
		files := envOf(worker)["PI_CONFIG_FILES"]
		if files == "" {
			t.Fatalf("the %s's PI_CONFIG_FILES is empty; the baseline pins a non-empty value", role)
		}
		prompt := appendSystemPromptOf(t, role, worker.Command)
		if prompt == "" {
			t.Fatalf("the %s's --append-system-prompt value is empty; the baseline pins a non-empty value", role)
		}
		baseline[string(role)] = launchBaseline{PiConfigFiles: files, AppendSystemPrompt: prompt}
	}
	if len(baseline) != len(baselineRoles()) {
		t.Fatalf("the baseline holds %d roles, want %d", len(baseline), len(baselineRoles()))
	}
	checkGolden(t, "launch-baseline-no-experiment", baseline)
}
