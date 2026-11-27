package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// twoExperiments are two experiments declared under the `experiments` key, each with one arm that
// names an overlay and one control arm that names none.
func twoExperiments(alpha, beta string) []config.Experiment {
	return []config.Experiment{
		{Name: "alpha", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Variants: []config.Variant{
			{Name: "on", Weight: 1, ConfigFiles: []string{alpha}}, {Name: "off", Weight: 1},
		}},
		{Name: "beta", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Variants: []config.Variant{
			{Name: "on", Weight: 1, ConfigFiles: []string{beta}}, {Name: "off", Weight: 1},
		}},
	}
}

// The tmux boot gate reads one whole PI_CONFIG_FILES value per role per declared arm, not one
// value holding two arms' files: the union of two arms is a configuration no launch produces, so
// composing it would ask Oh My Pi a question no pane ever asks. A deployment that declares its
// arms under the `experiments` key is gated exactly as one that writes the `memory` block is, and
// the refusal names the arm's own key. An arm that names no overlay adds no value, since its
// panes start on the role's own files.
func TestTheTmuxBootGateReadsOneValuePerRolePerDeclaredArm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	// The stub refuses two shapes: any value naming both arms' overlays, which no pane is ever
	// given, and the beta arm's overlay beside the tester's own file.
	script := "#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in *alpha.yml*beta.yml*)\n" +
		"  echo \"error: two arms in one value: $PI_CONFIG_FILES\" >&2; exit 1;; esac\n" +
		"case \"$PI_CONFIG_FILES\" in *tester.yml*beta.yml*)\n" +
		"  echo \"error: Failed to parse config overlay $PI_CONFIG_FILES: SyntaxError\" >&2; exit 1;; esac\n" +
		"echo '{\"value\": false}'\n"
	if err := execfile.Write(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tester, alpha, beta := filepath.Join(dir, "tester.yml"), filepath.Join(dir, "alpha.yml"), filepath.Join(dir, "beta.yml")
	for _, file := range []string{tester, alpha, beta} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := capabilities.Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	cfg := config.Config{
		Project: "test",
		Runtime: config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"test": config.Project{
			Experiments: twoExperiments(alpha, beta),
		}},
	}
	// Each arm alone passes, and the union the stub refuses is never composed.
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err != nil {
		t.Fatalf("checkPaneRoleOverlays = %v, want nil: each arm is read on its own, never two in one value", err)
	}

	// The beta arm beside the tester's own file, which the stub refuses: the refusal names both
	// keys, the arm's by its own spelling under the `experiments` key.
	cfg.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {tester}}
	err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger())
	want := "boot gate: role_config_files.tester with projects.test.experiments.beta.variants.on.config_files: the tester's Oh My Pi would not start: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("checkPaneRoleOverlays = %v, want it to start %q", err, want)
	}
	if strings.Contains(err.Error(), "two arms in one value") {
		t.Errorf("the gate composed the union of two arms: %v", err)
	}
}

// The boot read of a declared arm's overlays refuses a file no pane could read, naming the arm's
// own key under the `experiments` key and the file, as it does for the `memory` block's own.
func TestDeclaredArmOverlaysNoPaneCouldReadRefuseTheBoot(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Project: "test",
		Runtime: config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"test": config.Project{
			Experiments: twoExperiments("/etc/legion-alpha/alpha.yml", "/etc/legion-beta/absent.yml"),
		}},
	}
	err := checkRoleConfigFiles(cfg)
	want := "projects.test.experiments.alpha.variants.on.config_files names /etc/legion-alpha/alpha.yml, which a role drawn into that arm could not read: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("checkRoleConfigFiles = %v, want it to start %q", err, want)
	}
}

// The launch reader makes no query for a deployment none of whose declared experiments names a
// file: there is nothing for a drawn unit to run with, whichever arm it drew, so reading the
// record could only answer what the launch already knows. That is the deployment this fleet runs
// — no `memory` block and no experiment of its own, which declares the memory experiment at mode
// off and nothing else (config.Project.ExperimentsDeclared) — and it is what keeps a worker
// spawn off the database, the transaction per spawn this guard was written for.
//
// The store is nil, so any read of it would panic: the closure's answer is proof it read
// nothing. The second half declares one arm that does name a file, over the same nil store, and
// recovers the panic that read makes — without it the first half would pass over a reader that
// never queries at all.
func TestLaunchVariantsReadsNoRecordWhereNoDeclaredArmNamesAFile(t *testing.T) {
	t.Parallel()
	const tree, issue = "LEGION-1", "LEGION-2"
	declared := config.Project{}.ExperimentsDeclared("projects.test")
	if len(declared) != 1 || declared[0].Name != config.MemoryExperimentName {
		t.Fatalf("a project that writes no experiment declares %+v, want the memory experiment alone", declared)
	}
	arms, err := launchVariants(nil, declared, "/state")(context.Background(), tree, issue, claim.RoleImplementer)
	if err != nil || arms.Variants != nil || arms.Instructions != nil {
		t.Fatalf("launchVariants over a fileless declaration = %+v, %v, want no arm and no error", arms, err)
	}

	naming := []config.Experiment{{Name: "alpha", Mode: config.ExperimentRandom, Unit: config.ExperimentTree,
		Variants: []config.Variant{{Name: "on", Weight: 1, ConfigFiles: []string{"/etc/legion-alpha/alpha.yml"}}, {Name: "off", Weight: 1}}}}
	read := func() (queried bool) {
		defer func() { queried = recover() != nil }()
		_, _ = launchVariants(nil, naming, "/state")(context.Background(), tree, issue, claim.RoleImplementer)
		return false
	}
	if !read() {
		t.Fatal("launchVariants read no record for a declaration whose arm names a file, want the record read")
	}
}

// An experiment that names its roles reaches the launches of those roles alone: in a tree drawn
// into its arm, the planner and the implementer launch with the arm's overlays and instructions,
// and the reviewer, the tester, the architect and the merger launch on the deployment's own
// settings, so the roles that judge a treated tree's work are the control's. The tree keeps its
// arm throughout: the same record answers every role. An experiment that names no roles reaches
// every role, as it did before the key existed.
func TestAnExperimentReachesOnlyTheRolesItNames(t *testing.T) {
	t.Parallel()
	const tree, issue = "LEGION-1", "LEGION-2"
	recorded := map[string][]record.Assignment{tree: {{Experiment: "cheaper-model", Variant: "open"}}}
	declare := func(roles []claim.Role) []config.Experiment {
		return []config.Experiment{{Name: "cheaper-model", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Roles: roles,
			Variants: []config.Variant{
				{Name: "open", Weight: 1, ConfigFiles: []string{"/etc/legion-experiments/open.yml"}, InstructionsPath: "/etc/legion-experiments/open.md"},
				{Name: "control", Weight: 1},
			}}}
	}
	spawn := func(declared []config.Experiment, role claim.Role) runtime.SpawnSpec {
		s := memorySpecs(t, "")
		launch := experiment.ForLaunch(declared, "/state")
		s.variants = func(_ context.Context, tree, issue string, role claim.Role) (experiment.Arms, error) {
			return launch.Arms(recorded, tree, issue, role), nil
		}
		return memorySpawn(t, s, tree, issue, role)
	}
	wantVariants := func(roles []claim.Role) []runtime.Variant {
		return []runtime.Variant{{Experiment: "cheaper-model", Name: "open", ConfigFiles: []string{"/etc/legion-experiments/open.yml"}, Roles: roles}}
	}
	wantInstructions := []string{"/state/experiments/cheaper-model/open.md"}

	roles := []claim.Role{claim.RolePlanner, claim.RoleImplementer}
	named := declare(roles)
	for _, role := range claim.Roles {
		spec := spawn(named, role)
		treated := role == claim.RolePlanner || role == claim.RoleImplementer
		switch {
		case treated && (!reflect.DeepEqual(spec.Variants, wantVariants(roles)) || !reflect.DeepEqual(spec.Prompt.VariantInstructionsPaths, wantInstructions)):
			t.Errorf("the %s of a treated tree: variants = %+v, instructions = %q; want the arm %+v and %q",
				role, spec.Variants, spec.Prompt.VariantInstructionsPaths, wantVariants(roles), wantInstructions)
		case !treated && (len(spec.Variants) != 0 || len(spec.Prompt.VariantInstructionsPaths) != 0):
			t.Errorf("the %s of a treated tree, a role the experiment leaves out: variants = %+v, instructions = %q; want neither",
				role, spec.Variants, spec.Prompt.VariantInstructionsPaths)
		}
	}

	for _, role := range claim.Roles {
		if spec := spawn(declare(nil), role); !reflect.DeepEqual(spec.Variants, wantVariants(nil)) {
			t.Errorf("the %s of a treated tree under an experiment that names no roles: variants = %+v, want the arm %+v",
				role, spec.Variants, wantVariants(nil))
		}
	}
}
