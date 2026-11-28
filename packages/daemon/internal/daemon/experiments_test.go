package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
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
	const issue = "LEGION-2"
	declared := config.Project{}.ExperimentsDeclared("projects.test")
	if len(declared) != 1 || declared[0].Name != config.MemoryExperimentName {
		t.Fatalf("a project that writes no experiment declares %+v, want the memory experiment alone", declared)
	}
	arms, err := launchVariants(nil, declared, "/state", "LEGION")(context.Background(), issue, claim.RoleImplementer)
	if err != nil || arms.Variants != nil || arms.Instructions != nil {
		t.Fatalf("launchVariants over a fileless declaration = %+v, %v, want no arm and no error", arms, err)
	}

	naming := []config.Experiment{{Name: "alpha", Mode: config.ExperimentRandom, Unit: config.ExperimentTree,
		Variants: []config.Variant{{Name: "on", Weight: 1, ConfigFiles: []string{"/etc/legion-alpha/alpha.yml"}}, {Name: "off", Weight: 1}}}}
	read := func() (queried bool) {
		defer func() { queried = recover() != nil }()
		_, _ = launchVariants(nil, naming, "/state", "LEGION")(context.Background(), issue, claim.RoleImplementer)
		return false
	}
	if !read() {
		t.Fatal("launchVariants read no record for a declaration whose arm names a file, want the record read")
	}
}

// A launch of an issue the daemon holds no record of takes each tree experiment's arm from the
// nearest issue above it on its Dispatch chain that is in Legion's work and holds one, and runs in
// no arm where no such issue holds one: where the copy holds no row of it, where its parent is no
// issue the daemon records, where the issue above it holds an issue-unit row alone, which is that
// issue's own answer, and where the issue above it that holds the draw is out of Legion's work. A
// recorded issue reads its own rows alone, so one that holds none runs in no arm even under an
// issue that holds the draw. The first row, an unrecorded issue under that issue, takes its arm,
// so the rows that answer no arm are not passed by a reader that reads nothing.
func TestAnUnrecordedIssueRunsInNoArmWhereNoIssueAboveItInWorkHoldsOne(t *testing.T) {
	t.Parallel()
	const project = "LEGION"
	declared := []config.Experiment{
		{Name: "plan-length", Mode: config.ExperimentOn, Unit: config.ExperimentTree, OnVariant: "long", Variants: []config.Variant{
			{Name: "long", Weight: 1, ConfigFiles: []string{"/etc/legion-experiments/long.yml"}}, {Name: "short", Weight: 1}}},
		{Name: "handoff-note", Mode: config.ExperimentOn, Unit: config.ExperimentIssue, OnVariant: "long", Variants: []config.Variant{
			{Name: "long", Weight: 1, ConfigFiles: []string{"/etc/legion-experiments/handoff.yml"}}, {Name: "short", Weight: 1}}},
	}
	drawn := time.Date(2026, 11, 24, 4, 34, 0, 0, time.UTC)
	treeDraw := record.Assignment{Experiment: "plan-length", Unit: string(config.ExperimentTree), Variant: "long", Mode: string(config.ExperimentOn), DrawnAt: &drawn}
	issueDraw := record.Assignment{Experiment: "handoff-note", Unit: string(config.ExperimentIssue), Variant: "long", Mode: string(config.ExperimentOn), DrawnAt: &drawn}
	st := migratedStore(t, testDSN(t))
	records := record.NewStore()
	if err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, recorded := range []struct {
			key    string
			at     phase.Phase
			status string
			rows   []record.Assignment
		}{
			{"LEGION-1", phase.Implementing, "in_progress", []record.Assignment{treeDraw}},
			{"LEGION-2", phase.Implementing, "in_progress", []record.Assignment{issueDraw}},
			{"LEGION-3", phase.Done, "done", []record.Assignment{treeDraw}},
			{"LEGION-4", phase.Implementing, "in_progress", nil},
		} {
			if err := records.PutIssue(ctx, tx, record.Issue{Key: recorded.key, Project: project, Tree: recorded.key, Title: recorded.key,
				Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Phase: recorded.at, Generation: 1,
				Status: recorded.status, Rank: "A"}); err != nil {
				return err
			}
			if _, err := records.PutAssignments(ctx, tx, recorded.key, recorded.rows); err != nil {
				return err
			}
		}
		for _, copied := range []struct{ key, parent string }{
			{"LEGION-1", ""}, {"LEGION-2", ""}, {"LEGION-3", ""}, {"LEGION-4", "LEGION-1"}, {"LEGION-5", ""},
			{"LEGION-10", "LEGION-1"}, {"LEGION-12", "LEGION-5"}, {"LEGION-13", "LEGION-2"}, {"LEGION-14", "LEGION-3"},
		} {
			seedCopyRow(t, tx, copied.key, copied.parent, "in_progress", 1)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the records and the copy: %v", err)
	}

	launch := launchVariants(st, declared, "/state", project)
	for _, row := range []struct {
		name, issue string
		want        []string
	}{
		{"an unrecorded issue under an issue in work that holds the draw", "LEGION-10", []string{"plan-length=long"}},
		{"an unrecorded issue the copy holds no row of", "LEGION-11", nil},
		{"an unrecorded issue under an issue the daemon does not record", "LEGION-12", nil},
		{"an unrecorded issue under an issue that holds an issue-unit row alone", "LEGION-13", nil},
		{"an unrecorded issue under an issue out of Legion's work that holds the draw", "LEGION-14", nil},
		{"a recorded issue that holds no row, under an issue in work that holds the draw", "LEGION-4", nil},
	} {
		arms, err := launch(context.Background(), row.issue, claim.RoleImplementer)
		var got []string
		for _, variant := range arms.Variants {
			got = append(got, variant.Experiment+"="+variant.Name)
		}
		if err != nil || !reflect.DeepEqual(got, row.want) {
			t.Errorf("%s: %s launches in %v, %v; want %v and no error", row.name, row.issue, got, err, row.want)
		}
	}
}

// An experiment that names its roles reaches the launches of those roles alone: in an issue drawn
// into its arm, the planner and the implementer launch with the arm's overlays and instructions,
// and the reviewer, the tester, the architect and the merger launch on the deployment's own
// settings, so the roles that judge a treated issue's work are the control's. The issue keeps its
// arm throughout: its own record answers every role. An experiment that names no roles reaches
// every role, as it did before the key existed.
func TestAnExperimentReachesOnlyTheRolesItNames(t *testing.T) {
	t.Parallel()
	const tree, issue = "LEGION-1", "LEGION-2"
	recorded := map[string][]record.Assignment{issue: {{Experiment: "cheaper-model", Variant: "open"}}}
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
		s.variants = func(_ context.Context, issue string, role claim.Role) (experiment.Arms, error) {
			return launch.Arms(recorded, issue, role), nil
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
