package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// `models` lists the models a role may name, in the file's order, and the file that names none
// lists none; each value the key cannot take is refused naming the key and the entry at fault,
// by the daemon's load and the check's alike, and warns nothing: the release before refuses the
// key itself, so no file an earlier release took holds one. A typo that a load leaving the entry
// out would take, `[m-a m-b]` for `[m-a, m-b]`, would list no model, and the boot would stop each
// task whose plan role names one (workflow's TestATypoInModelsRefusesTheLoadAndStopsNoTask).
func TestTheModelsKeyIsReadAndRefused(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		want      []string
		refusal   string
	}{
		{name: "two models", key: "models: [m-one, m-two]\n", want: []string{"m-one", "m-two"}},
		{name: "no key"},
		{name: "a scalar", key: "models: m-one\n", refusal: "models must be a list of model names, e.g. [anthropic/claude-sonnet-4]"},
		{name: "a number", key: "models: [7]\n", refusal: "models[0] must be a model's name"},
		{name: "a mapping entry", key: "models: [m-one, {name: m-two}]\n", refusal: "models[1] must be a model's name"},
		{name: "an empty entry", key: "models: [\"\"]\n", refusal: "models[0] is empty"},
		{name: "white space", key: "models: [\"a b\"]\n", refusal: `models[0] is "a b", which holds white space: a model's name is one word`},
		{name: "a missing comma", key: "models: [m-a m-b]\n", refusal: `models[0] is "m-a m-b", which holds white space: a model's name is one word`},
		{name: "a model named twice", key: "models: [m, m]\n", refusal: "models names m twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
				cfg, err := load(writeConfigFile(t, tc.key+minimalFile), noEnv)
				if tc.refusal != "" {
					if err == nil || !strings.Contains(err.Error(), tc.refusal) {
						t.Fatalf("%s with %q = %v (warnings %q), want a refusal naming %q", name, tc.key, err, cfg.Warnings, tc.refusal)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s with %q: %v", name, tc.key, err)
				}
				if !slices.Equal(cfg.Models, tc.want) || (tc.want == nil) != (cfg.Models == nil) || len(cfg.Warnings) != 0 {
					t.Fatalf("%s with %q: Models = %#v, warnings %q; want %#v and no warning", name, tc.key, cfg.Models, cfg.Warnings, tc.want)
				}
			}
		})
	}
}

// The deployment a plan is judged against lists its models, `models` then each model a role of
// `roles` names that `models` lacks, so a deployment that set a role's model before `models`
// existed lists that model; it gives each role the harness `worker_backends` names, Oh My Pi where
// it names none, and each role of `roles` its model; and the owner's role is the architect's, on
// the default model and the architect's harness. The names no role of the deployment takes are the
// one list a role of a plan cannot take either (plan.LegionNames), and `owner` is in neither.
func TestTheDeploymentListsItsModelsAndHarnesses(t *testing.T) {
	const checker = "roles: {checker: {type: testing, prompt_file: checker.md, model: m-two}}\n"
	cfg, err := Load(rolesFile(t, "models: [m-one]\n"+checker+"worker_backends: {implementer: claude}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.ListedModels(), []string{"m-one", "m-two"}; !slices.Equal(got, want) {
		t.Errorf("ListedModels() = %v, want %v", got, want)
	}
	deployment := cfg.Deployment()
	if got, want := deployment.Models, []string{"m-one", "m-two"}; !slices.Equal(got, want) {
		t.Errorf("Deployment().Models = %v, want %v", got, want)
	}
	if got := deployment.Roles["implementer"].Backend; got != "claude" {
		t.Errorf("Deployment().Roles[implementer].Backend = %q, want claude", got)
	}
	if got, want := deployment.Roles["checker"], (plan.RoleDef{Name: "checker", Type: plan.TypeTesting, Model: "m-two", Backend: "omp"}); got != want {
		t.Errorf("Deployment().Roles[checker] = %+v, want %+v", got, want)
	}
	if got, want := deployment.Roles["reviewer"], (plan.RoleDef{Name: "reviewer", Type: plan.TypeReview, Backend: "omp"}); got != want {
		t.Errorf("Deployment().Roles[reviewer] = %+v, want %+v", got, want)
	}

	cfg, err = Load(rolesFile(t, "models: [m-two]\n"+checker, "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load with the role's model listed: %v", err)
	}
	if got, want := cfg.ListedModels(), []string{"m-two"}; !slices.Equal(got, want) {
		t.Errorf("ListedModels() of a file that lists the role's model = %v, want %v", got, want)
	}

	for _, name := range plan.LegionNames() {
		_, err := Load(rolesFile(t, "roles: {"+name+": {type: testing, prompt_file: checker.md}}\n", "checker.md"), noEnv)
		if want := "roles." + name + " names a role of Legion's own: a deployment defines roles of its own names"; err == nil || err.Error() != want {
			t.Errorf("Load of roles.%s = %v, want %q", name, err, want)
		}
		p := plan.Plan{Tasks: []plan.Task{{ID: "a", Steps: []plan.Step{{Role: "implementer"}}}}, Roles: []plan.PlanRole{{Name: name, Type: plan.TypeTesting}}}
		want := []plan.Refusal{{Reason: "the plan defines the role " + name + ", which the deployment or Legion defines already; a role of a plan takes a name of its own."}}
		if got := plan.Validate(p, deployment); !reflect.DeepEqual(got, want) {
			t.Errorf("Validate of a plan role %s = %+v, want %+v", name, got, want)
		}
	}
	cfg, err = Load(rolesFile(t, "roles: {owner: {type: testing, prompt_file: owner.md}}\n", "owner.md"), noEnv)
	if err != nil {
		t.Fatalf("Load of roles.owner: %v", err)
	}
	if _, ok := cfg.Deployment().Roles["owner"]; !ok {
		t.Error("Deployment() of roles.owner holds no role owner")
	}
}
