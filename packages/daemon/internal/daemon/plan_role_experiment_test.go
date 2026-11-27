package daemon

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A model a role names wins over an experiment, and the experiment still reaches the role: an
// issue whose plan defines prober on model m-plan, drawn into an arm of an experiment that names
// no roles, launches prober with the model m-plan and with the arm, whose overlay each runtime
// names last in PI_CONFIG_FILES (runtime.SpawnSpec.Variants). The control is a prober of no model,
// which launches on the default model (a spec's Model "", which each runtime passes as
// `--model @default`) with the same arm.
func TestAPlanRolesModelWinsOverAnExperimentArmThatStillReachesIt(t *testing.T) {
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	arm := runtime.Variant{Experiment: "tone", Name: "terse", ConfigFiles: []string{"/state/experiments/tone/terse.yml"}}
	for _, tc := range []struct{ name, model string }{
		{"a prober that names m-plan", "m-plan"},
		{"the control: a prober that names no model", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := specs{stateDir: t.TempDir(), project: "s1", prompts: composer, appOwners: noAppOwners,
				planRole: func(_ context.Context, issue string, role claim.Role) (record.PlanRole, bool, error) {
					return record.PlanRole{Issue: issue, Role: string(role), Type: "testing", Model: tc.model, Prompt: "# Prober\n\nProbe the change."},
						issue == "LEGION-43" && role == "prober", nil
				},
				variants: func(_ context.Context, _, issue string, role claim.Role) (experiment.Arms, error) {
					if issue != "LEGION-43" || role != "prober" {
						t.Errorf("the arms were read for %s on %s, want the prober on LEGION-43", role, issue)
					}
					return experiment.Arms{Variants: []runtime.Variant{arm}}, nil
				}}
			token, err := claim.NewToken("s1", "LEGION-43", "prober")
			if err != nil {
				t.Fatal(err)
			}
			spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Issue: "LEGION-43", Tree: "LEGION-42", Role: "prober"})
			if err != nil {
				t.Fatalf("the prober's spawn spec: %v", err)
			}
			if spec.Model != tc.model || !spec.PlanRole {
				t.Errorf("the prober launches on the model %q as the plan's role %t, want %q as the plan's role", spec.Model, spec.PlanRole, tc.model)
			}
			if !reflect.DeepEqual(spec.Variants, []runtime.Variant{arm}) {
				t.Errorf("the prober launches in the arms %+v, want the drawn arm %+v", spec.Variants, arm)
			}
		})
	}
}
