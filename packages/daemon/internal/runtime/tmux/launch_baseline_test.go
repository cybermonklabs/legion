package tmux

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// updateGolden rewrites the goldens this package pins, as the sandbox runtime's -update does.
var updateGolden = flag.Bool("update", false, "rewrite the launch goldens this package pins")

// baselineRoleFiles are the fixture's role_config_files: one shared overlay and one of the role's
// own, for each of the six workflow roles and the daemon-launched controller. Every role is named,
// and named with two files in a fixed order, so the baseline golden below catches a later change
// that drops a role's overlays or reorders them.
var baselineRoleFiles = map[claim.Role][]string{
	claim.RoleArchitect:   {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/architect.yml"},
	claim.RolePlanner:     {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/planner.yml"},
	claim.RoleImplementer: {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/implementer.yml"},
	claim.RoleTester:      {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/tester.yml"},
	claim.RoleReviewer:    {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/reviewer.yml"},
	claim.RoleMerger:      {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/merger.yml"},
	claim.RoleController:  {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/controller.yml"},
}

// baselineOperatorOverlay is the operator's own Oh My Pi overlay every pane names first
// (runtime.tmux.pi_config_files), as memoryRuntime's fixture names one.
const baselineOperatorOverlay = "/etc/legion-operator/overlay.yml"

// baselineInvocation is the Oh My Pi boot resolved for every pane here, and baselineInstructions
// the deployment instructions file every pane's prompt word ends with.
const (
	baselineInvocation   = "/opt/bin/omp"
	baselineInstructions = "/state/deployment-instructions.md"
)

// baselineRoles are the roles the baseline covers: the six of the workflow, in the order the
// workflow reaches them, then the project controller, which is none of them.
func baselineRoles() []claim.Role {
	return slices.Concat(claim.Roles, []claim.Role{claim.RoleController})
}

// baselineSpec is role's launch under a deployment that declares no memory block at all —
// memoryRuntime's shape with no memory overlays: testSpec's pane for a workflow role, the
// controller's own launch, which runs in no tree, for the controller. Neither carries a variant
// (SpawnSpec.Variants) and neither names variant instructions. The prompt files are named by
// fixed paths rather than a temporary directory's, since nothing here reads them: the pane's own
// shell does, as `$(cat …)` inside the prompt word.
func baselineSpec(t *testing.T, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	spec := testSpec()
	spec.Prompt = runtime.PromptParts{
		RolePromptPaths:            []string{"/roles/" + string(role) + ".md"},
		DeploymentInstructionsPath: baselineInstructions,
	}
	if role == claim.RoleController {
		spec.Claim, spec.Role = claim.ControllerToken(spec.Project), role
		spec.Tree, spec.Issue = "", ""
		return spec
	}
	token, err := claim.NewToken(spec.Project, spec.Issue, role)
	if err != nil {
		t.Fatal(err)
	}
	spec.Claim, spec.Role = token, role
	spec.Prompt.Addressing = "Legion addressing: your role topic is `notifications.role." + string(token) + "`."
	return spec
}

// launchBaseline is what the golden records for one role: the two values a launch hands the pane's
// Oh My Pi.
type launchBaseline struct {
	PiConfigFiles      string `json:"PI_CONFIG_FILES"`
	AppendSystemPrompt string `json:"--append-system-prompt"`
}

// pairValue is the value of the pane variable named, out of the NAME=value pairs of panePairs or
// of PaneVariables.
func pairValue(variables []string, name string) (string, bool) {
	for _, variable := range variables {
		if got, value, ok := strings.Cut(variable, "="); ok && got == name {
			return value, true
		}
	}
	return "", false
}

// The launch bytes a deployment that declares no experiment produces today, for every role Legion
// launches in a pane: the golden pins two values per role — the pane's PI_CONFIG_FILES, the
// operator's own overlays first and then the role's own, and the one --append-system-prompt word
// of the pane's command, as the pane's shell receives it, its `$(cat …)` fragments unexpanded.
// Both are kept non-empty for all seven roles by the fixture (baselineOperatorOverlay and
// baselineRoleFiles), so a later change that drops an overlay, reorders the list, or moves a
// prompt fragment shows here as a diff to read.
//
// The pairs are read from panePairs, the pane's own -e pairs, and are proven to be what
// PaneVariables tells a caller that runs Oh My Pi under the pane's environment itself; the prompt
// word is read from the command the Oh My Pi launcher gives the shim (ompLauncher.Launch,
// innerCommand). Nothing here starts tmux, so the capture runs on a host that has no tmux binary.
func TestLaunchBaselineOfADeploymentWithNoExperiment(t *testing.T) {
	launcher := NewOmpLauncher(nil, baselineInvocation)
	baseline := map[string]launchBaseline{}
	for _, role := range baselineRoles() {
		stateDir := t.TempDir()
		spec := baselineSpec(t, role)
		in := PaneInputs{
			StateDir:        stateDir,
			Workspace:       filepath.Join(stateDir, "workspaces", spec.Issue),
			DaemonURL:       "http://127.0.0.1:13370",
			EnvoyURL:        "http://127.0.0.1:9020",
			PiConfigFiles:   []string{baselineOperatorOverlay},
			RoleConfigFiles: baselineRoleFiles[role],
		}
		files := secretFiles(stateDir, spec)
		pairs := panePairs(spec, in, files)
		variables, err := PaneVariables(spec, in, files)
		if err != nil {
			t.Fatalf("PaneVariables for the %s: %v", role, err)
		}
		told, ok := pairValue(pairs, "PI_CONFIG_FILES")
		if !ok || told == "" {
			t.Fatalf("the %s's pairs name no PI_CONFIG_FILES value: %q", role, pairs)
		}
		if got, ok := pairValue(variables, "PI_CONFIG_FILES"); !ok || got != told {
			t.Fatalf("PaneVariables tells the %s PI_CONFIG_FILES = %q (set %v), want the pane's own %q", role, got, ok, told)
		}
		inner, _, err := launcher.Launch(spec, Host{StateDir: stateDir, Legion: "/opt/legion/legion"})
		if err != nil {
			t.Fatalf("the %s's pane command: %v", role, err)
		}
		_, prompt, ok := strings.Cut(inner, "--append-system-prompt ")
		if !ok || prompt == "" {
			t.Fatalf("the %s's pane command names no --append-system-prompt word:\n%s", role, inner)
		}
		baseline[string(role)] = launchBaseline{PiConfigFiles: told, AppendSystemPrompt: prompt}
	}
	if len(baseline) != len(baselineRoles()) {
		t.Fatalf("the baseline holds %d roles, want %d", len(baseline), len(baselineRoles()))
	}
	checkGolden(t, "launch-baseline-no-experiment", baseline)
}

// checkGolden compares value, encoded as the goldens are, with testdata/golden/<name>.json, or
// writes it there under -update.
func checkGolden(t *testing.T, name string, value any) {
	t.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	encoded := buffer.Bytes()
	path := filepath.Join("testdata", "golden", name+".json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/runtime/tmux/ -update`): %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is stale\n got: %s\nwant: %s", path, encoded, want)
	}
}
