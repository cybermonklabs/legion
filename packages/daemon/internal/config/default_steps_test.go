package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// defaultStepsFile is minimalFile with a second project, ACME, whose block is acme.
func defaultStepsFile(acme string) string {
	return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {}\n  ACME: "+acme+"\n", 1)
}

// todaysSteps is today's line up to the owner's acceptance, as a plan writes it.
func todaysSteps(t *testing.T) []plan.Step {
	t.Helper()
	steps, err := plan.ParseSteps("planner implementer tester reviewer acceptance:owner")
	if err != nil {
		t.Fatalf("ParseSteps: %v", err)
	}
	return steps
}

// A project that names no `default_steps` runs today's line up to the owner's acceptance,
// and so does a Project the loader never read, such as one a test builds, since the unset field
// resolves to that line rather than to no step.
func TestDefaultStepsDefaultToTodaysLine(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, defaultStepsFile("{}")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, key := range []string{"ACME", "DEMO"} {
		project := cfg.Projects[key]
		if project.DefaultSteps != nil {
			t.Fatalf("%s.DefaultSteps = %+v, want the unset field", key, project.DefaultSteps)
		}
		if got := project.RunsByDefault(); !reflect.DeepEqual(got, todaysSteps(t)) {
			t.Fatalf("%s.RunsByDefault() = %+v, want %+v", key, got, todaysSteps(t))
		}
	}
	if got := (Project{}).RunsByDefault(); !reflect.DeepEqual(got, todaysSteps(t)) {
		t.Fatalf("Project{}.RunsByDefault() = %+v, want today's line", got)
	}
	// Each answer is the caller's own: changing one leaves the next whole.
	first := (Project{}).RunsByDefault()
	first[0].Role = "changed"
	if got := (Project{}).RunsByDefault(); got[0].Role != "planner" {
		t.Fatalf("a caller that changed one answer changed the next: %+v", got)
	}
}

// `default_steps` is read with the plan's grammar and held to the checks a plan of one task with
// those steps passes, so the steps a task with none runs are steps Legion carries out. Each
// refusal names the key, then gives each reason of each check, in the plan's own words.
func TestDefaultStepsAreReadAndRefused(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, defaultStepsFile("{default_steps: [implementer, reviewer]}")), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []plan.Step{{Role: "implementer"}, {Role: "reviewer"}}
	if got := cfg.Projects["ACME"].DefaultSteps; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSteps = %+v, want %+v", got, want)
	}
	if got := cfg.Projects["ACME"].RunsByDefault(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RunsByDefault() = %+v, want %+v", got, want)
	}
	if got := cfg.Projects["DEMO"].RunsByDefault(); !reflect.DeepEqual(got, todaysSteps(t)) {
		t.Fatalf("DEMO, which names none, runs %+v, want today's line", got)
	}

	// The steps in another order than today's load as the list writes them.
	cfg, err = Load(writeConfigFile(t, defaultStepsFile("{default_steps: [implementer, reviewer, tester]}")), noEnv)
	if err != nil {
		t.Fatalf("Load([implementer, reviewer, tester]): %v", err)
	}
	want = []plan.Step{{Role: "implementer"}, {Role: "reviewer"}, {Role: "tester"}}
	if got := cfg.Projects["ACME"].DefaultSteps; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSteps = %+v, want %+v", got, want)
	}

	const yet = " This release of Legion does not carry it out yet."
	for _, tc := range []struct {
		name, value, refusal string
	}{
		{name: "no step", value: "[]", refusal: "projects.ACME.default_steps names no step"},
		{name: "a role nobody defines", value: "[implementer, checker]",
			refusal: "projects.ACME.default_steps: step 2 of task default names the role checker, which neither the deployment nor the plan defines."},
		{name: "a step that reads a pull request before the implementer", value: "[reviewer, implementer]",
			refusal: "projects.ACME.default_steps: step 1 of task default names reviewer, and no step before it names the role implementer, whose pull request reviewer reads." + yet},
		{name: "a cause of each check", value: "[checker, reviewer]",
			refusal: "projects.ACME.default_steps: task default has no step implementer." + yet +
				" step 2 of task default names reviewer, and no step before it names the role implementer, whose pull request reviewer reads." + yet +
				" step 1 of task default names the role checker, which neither the deployment nor the plan defines."},
		{name: "a key", value: "[implementer, reviewer:r]",
			refusal: "projects.ACME.default_steps: step 2 of task default names the key r." + yet},
		{name: "a word outside the grammar", value: "[implementer, Reviewer]",
			refusal: `projects.ACME.default_steps: the step "Reviewer" is neither a role nor a role:key, where each matches [a-z][a-z0-9-]*`},
		{name: "two steps in one entry", value: `["implementer reviewer"]`,
			refusal: `projects.ACME.default_steps names "implementer reviewer", which is not one step: each entry of the list is one step`},
		{name: "a word, not a list", value: "implementer",
			refusal: "projects.ACME.default_steps must be an array of non-empty strings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, defaultStepsFile("{default_steps: "+tc.value+"}")), noEnv)
			if err == nil || err.Error() != tc.refusal {
				t.Fatalf("Load error = %v, want %q", err, tc.refusal)
			}
			if !strings.HasPrefix(err.Error(), "projects.ACME.default_steps") {
				t.Fatalf("refusal %q does not name the key", err)
			}
		})
	}
}
