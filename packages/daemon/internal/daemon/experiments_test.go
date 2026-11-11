package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
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
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	// The stub refuses two shapes: any value naming both arms' overlays, which no pane is ever
	// given, and the beta arm's overlay beside the tester's own file.
	script := "#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in *alpha.yml*beta.yml*)\n" +
		"  echo \"error: two arms in one value: $PI_CONFIG_FILES\" >&2; exit 1;; esac\n" +
		"case \"$PI_CONFIG_FILES\" in *tester.yml*beta.yml*)\n" +
		"  echo \"error: Failed to parse config overlay $PI_CONFIG_FILES: SyntaxError\" >&2; exit 1;; esac\n" +
		"echo '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
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
		}.Declaring("projects.test")},
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
	cfg := config.Config{
		Project: "test",
		Runtime: config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"test": config.Project{
			Experiments: twoExperiments("/etc/legion-alpha/alpha.yml", "/etc/legion-beta/absent.yml"),
		}.Declaring("projects.test")},
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
	const tree, issue = "LEGION-1", "LEGION-2"
	declared := config.Project{}.Declaring("projects.test").ExperimentsDeclared()
	if len(declared) != 1 || declared[0].Name != config.MemoryExperimentName {
		t.Fatalf("a project that declares nothing declares %+v, want the memory experiment alone", declared)
	}
	arms, err := launchVariants(nil, declared, "/state")(context.Background(), tree, issue)
	if err != nil || arms.Variants != nil || arms.Instructions != nil {
		t.Fatalf("launchVariants over a fileless declaration = %+v, %v, want no arm and no error", arms, err)
	}

	naming := []config.Experiment{{Name: "alpha", Mode: config.ExperimentRandom, Unit: config.ExperimentTree,
		Variants: []config.Variant{{Name: "on", Weight: 1, ConfigFiles: []string{"/etc/legion-alpha/alpha.yml"}}, {Name: "off", Weight: 1}}}}
	read := func() (queried bool) {
		defer func() { queried = recover() != nil }()
		_, _ = launchVariants(nil, naming, "/state")(context.Background(), tree, issue)
		return false
	}
	if !read() {
		t.Fatal("launchVariants read no record for a declaration whose arm names a file, want the record read")
	}
}
