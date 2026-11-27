package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// A key of `default_steps` names one agent, and one agent runs on one model and one harness, as a
// plan's key does (plan.Validate). Legion took `default_steps` whose key names one agent for roles
// of two models or two harnesses before it judged a key by them, so this release takes them too
// and warns, naming the key, the entry, each step by its place and that the next release refuses
// it, and naming no task, since a project's list of steps is no task's: the step whose role runs on
// another model or harness than the key's first step runs as an agent of its own, the step with its
// key left out. Steps whose key names one model and one harness raise no warning.
func TestDefaultStepsWhoseKeyRunsOnTwoModelsOrHarnessesAreWarned(t *testing.T) {
	steps := func(project string) string {
		return strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n  DEMO: {default_steps: "+project+"}\n", 1)
	}
	const refuses = " This release runs step %d as an agent of its own, and the next release refuses it; give that step another key, or none, in legion.yaml"
	for _, tc := range []struct {
		name, file string
		want       []plan.Step
		warnings   []string
	}{
		{name: "two models", file: steps("[implementer:a, checker:a]") + checkerRole,
			want: []plan.Step{{Role: "implementer", Key: "a"}, {Role: "checker"}},
			warnings: []string{"projects.DEMO.default_steps[1] is checker:a: the key a names one agent for implementer at step 1 and checker at step 2, " +
				"whose roles run on two models (the default model, checker-model); one agent runs on one model." + fmt.Sprintf(refuses, 2)}},
		{name: "two harnesses", file: steps("[implementer:a, reviewer:a, tester:a]") + "worker_backends: {reviewer: claude}\n",
			want: []plan.Step{{Role: "implementer", Key: "a"}, {Role: "reviewer"}, {Role: "tester", Key: "a"}},
			warnings: []string{"projects.DEMO.default_steps[1] is reviewer:a: the key a names one agent for implementer at step 1 and reviewer at step 2, " +
				"whose roles run on two harnesses (omp, claude); one agent runs on one harness." + fmt.Sprintf(refuses, 2)}},
		{name: "one model and one harness", file: steps("[implementer:a, reviewer:a]"),
			want: []plan.Step{{Role: "implementer", Key: "a"}, {Role: "reviewer", Key: "a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
				cfg, err := load(configFileWith(t, tc.file, "checker.md"), noEnv)
				if err != nil {
					t.Fatalf("%s: %v, want the file loaded", name, err)
				}
				if got := cfg.Projects["DEMO"].DefaultSteps; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("%s: DefaultSteps = %v, want %v", name, got, tc.want)
				}
				if !slices.Equal(cfg.Warnings, tc.warnings) {
					t.Errorf("%s: Warnings = %#v, want %#v", name, cfg.Warnings, tc.warnings)
				}
			}
		})
	}
}
