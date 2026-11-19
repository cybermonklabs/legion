package sandbox

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
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

// baselineRuntime is the test runtime of a deployment that declares no experiment of its own and
// writes no `memory` block — memoryRuntime's shape with no declared variant, since that
// deployment's memory experiment names no file in either arm and so adds nothing a runtime reads
// — over the operator's pod, whose own PI_CONFIG_FILES entry every pod carries, with
// baselineRoleFiles as role_config_files.
func baselineRuntime(t *testing.T) *Runtime {
	t.Helper()
	return runtimeWithOptions(t, func(o *Options) {
		o.RoleConfigFiles = baselineRoleFiles
		o.ExperimentVariants = nil
	})
}

// baselineSpec is role's launch under that deployment: a role of the tree's root issue, or the
// project controller's own launch, which runs in no tree. Neither carries a variant
// (SpawnSpec.Variants) and neither names variant instructions, so each is a launch of a
// deployment that declares no experiment of its own, whose memory arms name no file.
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

// launchBaseline is what the golden records for one role: the values a launch hands the role's Oh
// My Pi — its PI_CONFIG_FILES, the path its --append-system-prompt names, and the text of the
// file at that path, which the start command carries for the launcher to write.
type launchBaseline struct {
	PiConfigFiles      string `json:"PI_CONFIG_FILES"`
	AppendSystemPrompt string `json:"--append-system-prompt"`
	SystemPrompt       string `json:"system-prompt.md"`
}

// systemPromptOf is the system prompt start hands its role's agent: path, the value of the one
// --append-system-prompt (Oh My Pi) or --append-system-prompt-file (Claude Code) word of its argv
// (launcherCommand, agentArgv), and text, the file of that name its Files carry, which the
// launcher writes at path before the agent starts. The path must name a file of the generation's
// own directory, the one directory the launcher writes Files into.
func systemPromptOf(t *testing.T, role claim.Role, start shimwire.LauncherStart) (path, text string) {
	t.Helper()
	i := slices.IndexFunc(start.Argv, func(word string) bool {
		return word == "--append-system-prompt" || word == "--append-system-prompt-file"
	})
	if i < 0 || i+1 >= len(start.Argv) {
		t.Fatalf("the %s's start command names no system prompt file: %q", role, start.Argv)
	}
	path = start.Argv[i+1]
	if filepath.Dir(path) != generationDir(start.Generation) {
		t.Fatalf("the %s's %s names %s, want a file of its generation's directory %s", role, start.Argv[i], path, generationDir(start.Generation))
	}
	text, carried := start.Files[filepath.Base(path)]
	if !carried {
		t.Fatalf("the %s's start command names %s and carries the files %v, none of them %s",
			role, path, slices.Sorted(maps.Keys(start.Files)), filepath.Base(path))
	}
	return path, text
}

// The launch bytes a deployment with no experiment of its own and no `memory` block produces
// today, for every role Legion launches: the golden pins three values per role — the role's
// PI_CONFIG_FILES, the operator's own overlays first and then the role's own; the path of the one
// --append-system-prompt word its Oh My Pi is started with; and the text of the system prompt
// file at that path, which its start command carries. Each is kept non-empty for all seven roles
// by the fixture (operatorPod's PI_CONFIG_FILES entry and baselineRoleFiles), so a later change
// that drops an overlay, reorders the list, moves the prompt file or moves a prompt fragment
// shows here as a diff to read.
//
// The values are read off workerOf's container and the launcher's start command rather than off
// manifestOf's Sandbox because none rides the pod manifest: the pod template carries the
// operator's PI_CONFIG_FILES entry alone and no prompt at all, since a role's own overlays and
// its prompt ride the launcher's start command (launcherCommand). workerOf is that whole process —
// the pod's container as the pod runs it, with the start command's argv and environment applied
// over it — and the start command's Files are what its launcher writes before that process
// starts, which is what a rewrite of the launch path must leave unchanged for such a deployment.
func TestLaunchBaselineOfADeploymentWithNoExperiment(t *testing.T) {
	r := baselineRuntime(t)
	baseline := map[string]launchBaseline{}
	for _, role := range baselineRoles() {
		spec := baselineSpec(t, role)
		worker := workerOf(t, r, spec)
		files := envOf(worker)["PI_CONFIG_FILES"]
		if files == "" {
			t.Fatalf("the %s's PI_CONFIG_FILES is empty; the baseline pins a non-empty value", role)
		}
		path, prompt := systemPromptOf(t, role, launcherCommand(launchOf(t, r, spec), r))
		if prompt == "" {
			t.Fatalf("the %s's system prompt file is empty; the baseline pins a non-empty value", role)
		}
		baseline[string(role)] = launchBaseline{PiConfigFiles: files, AppendSystemPrompt: path, SystemPrompt: prompt}
	}
	if len(baseline) != len(baselineRoles()) {
		t.Fatalf("the baseline holds %d roles, want %d", len(baseline), len(baselineRoles()))
	}
	checkGolden(t, "launch-baseline-no-experiment", baseline)
}
