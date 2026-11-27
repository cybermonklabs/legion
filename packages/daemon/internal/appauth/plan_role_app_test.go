package appauth

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// One function answers the App of every role from the role's definition: Legion's roles and the
// role acceptance by the switch, which nothing overrides; a role of the issue's plan by its type,
// the implement App for the type implementation and the review App for every other type, no type
// among them; and a role of the deployment by its github_app, where the issue's plan does not
// define its name. A role none of the three defines acts as no App. RoleApps.Of reads the plan of
// the claim's own issue for every role that is not Legion's, a role of the deployment's too.
func TestTheAppOfARoleIsItsDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     claim.Role
		deployed DeploymentApps
		planned  Planned
		want     AppRole
	}{
		{"a role of a plan of the type implementation", "coder", nil, Planned{Defines: true, Type: plan.TypeImplementation}, Implement},
		{"a role of a plan of the type testing", "prober", nil, Planned{Defines: true, Type: plan.TypeTesting}, Review},
		{"a role of a plan of a type no list holds", "odd", nil, Planned{Defines: true, Type: "fuzzing"}, Review},
		{"a role of a plan of no type", "blank", nil, Planned{Defines: true}, Review},
		{"a role of the plan over the deployment's of its name", "checker", DeploymentApps{"checker": Implement}, Planned{Defines: true, Type: plan.TypeTesting}, Review},
		{"a role of the deployment the plan does not define", "checker", DeploymentApps{"checker": Implement}, Planned{}, Implement},
		{"Legion's implementer over the plan", claim.RoleImplementer, nil, Planned{Defines: true, Type: plan.TypeTesting}, Implement},
		{"the role acceptance", "acceptance", nil, Planned{}, Review},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := AppRoleFor(tc.role, tc.deployed, tc.planned); err != nil || got != tc.want {
				t.Fatalf("AppRoleFor(%s) = %q, %v; want %q", tc.role, got, err, tc.want)
			}
		})
	}
	_, err := AppRoleFor("ghost", nil, Planned{})
	const want = "the role ghost is none of Legion's, and neither the deployment nor the issue's plan defines it"
	if !errors.Is(err, ErrRoleNotDefined) || err.Error() != want {
		t.Fatalf("AppRoleFor(ghost) = %v; want an error that is ErrRoleNotDefined and reads %q", err, want)
	}
	// Every name of Legion's has an App of the switch, whatever a plan or the deployment says of it.
	for _, name := range plan.LegionNames() {
		if _, err := AppRoleFor(claim.Role(name), DeploymentApps{claim.Role(name): Implement}, Planned{Defines: true, Type: plan.TypeTesting}); err != nil {
			t.Errorf("AppRoleFor(%s) = %v; want an App for every name of Legion's", name, err)
		}
	}

	type asked struct {
		issue string
		role  claim.Role
	}
	var calls []asked
	apps := RoleApps{Deployed: DeploymentApps{"checker": Review}, Plan: func(_ context.Context, issue string, role claim.Role) (Planned, error) {
		calls = append(calls, asked{issue, role})
		switch {
		case issue == "LEGION-43" && role == "coder":
			return Planned{Defines: true, Type: plan.TypeImplementation}, nil
		case issue == "LEGION-46" && role == "checker":
			return Planned{Defines: true, Type: plan.TypeImplementation}, nil
		}
		return Planned{}, nil
	}}
	ctx := context.Background()
	if got, err := apps.Of(ctx, "LEGION-43", "coder"); err != nil || got != Implement {
		t.Errorf("Of(LEGION-43, coder) = %q, %v; want implement", got, err)
	}
	if _, err := apps.Of(ctx, "LEGION-44", "coder"); !errors.Is(err, ErrRoleNotDefined) {
		t.Errorf("Of(LEGION-44, coder) = %v; want ErrRoleNotDefined: the role is LEGION-43's alone", err)
	}
	if got, err := apps.Of(ctx, "LEGION-43", "checker"); err != nil || got != Review {
		t.Errorf("Of(LEGION-43, checker) = %q, %v; want review, the deployment's", got, err)
	}
	if got, err := apps.Of(ctx, "LEGION-46", "checker"); err != nil || got != Implement {
		t.Errorf("Of(LEGION-46, checker) = %q, %v; want implement, the App of the type its plan gives checker", got, err)
	}
	if got, err := apps.Of(ctx, "LEGION-43", "acceptance"); err != nil || got != Review {
		t.Errorf("Of(LEGION-43, acceptance) = %q, %v; want review", got, err)
	}
	if got, err := apps.Of(ctx, "", "checker"); err != nil || got != Review {
		t.Errorf("Of of no issue, checker = %q, %v; want review, the deployment's", got, err)
	}
	for _, call := range calls {
		if call.role == "acceptance" || call.issue == "" {
			t.Errorf("Of read the plan of %q for %s, want no read for a role of Legion's or a claim of no issue", call.issue, call.role)
		}
	}
}
