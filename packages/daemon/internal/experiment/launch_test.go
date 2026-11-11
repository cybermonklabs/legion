package experiment

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// declaredPair is a tree experiment naming overlays and an issue experiment naming an
// instructions file, each with a control arm that names nothing.
func declaredPair() []config.Experiment {
	return []config.Experiment{
		{Name: "alpha", Unit: config.ExperimentTree, Variants: []config.Variant{
			{Name: "on", Weight: 1, ConfigFiles: []string{"/etc/legion-alpha/alpha.yml"},
				ConfigFilesField: "projects.TEST.experiments.alpha.variants.on.config_files"},
			{Name: "off", Weight: 1},
		}},
		{Name: "beta", Unit: config.ExperimentIssue, Variants: []config.Variant{
			{Name: "on", Weight: 1, InstructionsPath: "/etc/legion-beta/on.md"},
			{Name: "off", Weight: 1},
		}},
	}
}

// The keys a launch reads are the unit keys its declaration draws for: the tree's alone where
// every experiment draws for the tree, the issue's beside it where one draws for the issue, and
// none at all where no arm names a file, which is the declaration a launch can do nothing with.
func TestTheKeysReadAreTheUnitsTheDeclarationDrawsFor(t *testing.T) {
	const tree, issue = "LEGION-1", "LEGION-2"
	for _, tc := range []struct {
		name     string
		declared []config.Experiment
		want     []string
	}{
		{"a project that declares nothing", nil, nil},
		{"a declaration no arm of which names a file",
			config.Project{}.Declaring("projects.TEST").ExperimentsDeclared(), nil},
		{"a tree experiment naming a file", declaredPair()[:1], []string{tree}},
		{"an issue experiment beside it", declaredPair(), []string{tree, issue}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForLaunch(tc.declared, "/state").Keys(tree, issue); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Keys = %q, want %q", got, tc.want)
			}
		})
	}
	// A root's own launch reads its key once, since its issue is its tree.
	if got := ForLaunch(declaredPair(), "/state").Keys(tree, tree); !reflect.DeepEqual(got, []string{tree}) {
		t.Errorf("a root's Keys = %q, want %q alone", got, tree)
	}
}

// The arms are the rows the record holds for each unit, in the order the project's file declares
// the experiments: the tree's row for the tree experiment, the issue's own for the issue
// experiment, each arm carrying the overlays and the key the operator wrote them under, and each
// arm that names an instructions file carrying the daemon's copy of it under the state directory.
func TestTheArmsAreTheRecordedRowsInTheDeclaredOrder(t *testing.T) {
	const tree, issue = "LEGION-1", "LEGION-2"
	recorded := map[string][]record.Assignment{
		tree:  {{Experiment: "alpha", Variant: "on"}},
		issue: {{Experiment: "beta", Variant: "on"}},
	}
	arms := ForLaunch(declaredPair(), "/state").Arms(recorded, tree, issue)
	wantVariants := []runtime.Variant{
		{Experiment: "alpha", Name: "on", ConfigFiles: []string{"/etc/legion-alpha/alpha.yml"},
			ConfigFilesField: "projects.TEST.experiments.alpha.variants.on.config_files"},
		{Experiment: "beta", Name: "on"},
	}
	if !reflect.DeepEqual(arms.Variants, wantVariants) {
		t.Errorf("Variants = %+v, want %+v", arms.Variants, wantVariants)
	}
	if want := []string{"/state/experiments/beta/on.md"}; !reflect.DeepEqual(arms.Instructions, want) {
		t.Errorf("Instructions = %q, want %q", arms.Instructions, want)
	}
}

// A unit runs in no arm of an experiment it holds no row for, and in none of an arm the
// project's file no longer names: the launch names the files the file still holds.
func TestAnUnrecordedUnitAndAnArmTheFileDroppedRunInNoArm(t *testing.T) {
	const tree, issue = "LEGION-1", "LEGION-2"
	launch := ForLaunch(declaredPair(), "/state")
	for _, tc := range []struct {
		name     string
		recorded map[string][]record.Assignment
	}{
		{"no row at all", nil},
		{"a row at no arm, which a mode-off draw and a hand-over leave",
			map[string][]record.Assignment{tree: {{Experiment: "alpha"}}}},
		{"an arm the file no longer names",
			map[string][]record.Assignment{tree: {{Experiment: "alpha", Variant: "gone"}}}},
		{"the issue's own row against the tree's key",
			map[string][]record.Assignment{tree: {{Experiment: "beta", Variant: "on"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if arms := launch.Arms(tc.recorded, tree, issue); arms.Variants != nil || arms.Instructions != nil {
				t.Errorf("Arms = %+v, want none", arms)
			}
		})
	}
}
