package appauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// DeploymentApps is each role the deployment defines in its settings (`roles`) with the App its
// github_app names.
type DeploymentApps map[claim.Role]AppRole

// Planned is what the plan in force of a claim's issue says of a role: whether it defines the
// role, and the role's type there ("" where its block names none).
type Planned struct {
	Defines bool
	Type    plan.Type
}

// ErrRoleNotDefined is AppRoleFor's answer for a role that is none of Legion's and that neither
// the deployment nor the issue's plan defines: such a role acts as no App.
var ErrRoleNotDefined = errors.New("neither the deployment nor the issue's plan defines it")

// AppRoleFor is the one role-to-App decision, the one place a change of how a role's App is
// chosen goes. Whose definition the role's name takes is plan.SourceOf's, the rule every reader of
// a role asks: a name of Legion's own acts as the App this switch names for it, which neither the
// deployment's map nor a plan overrides; a role the issue's plan defines (planned) acts as the App
// of its type there, the implement App for the type implementation and the review App for every
// other type, no type among them; a role of the deployment acts as the App deployed names for it;
// and a role nobody defines is ErrRoleNotDefined. Of Legion's names, the roles that can write an
// issue branch use implement, and every verdict-only role uses review: the project controller and
// the role acceptance, the fresh agent that reads the whole of a task's work, act as the review App
// too (neither writes an issue branch, and a review never comes from the App that wrote the
// change). Keep the switch exhaustive over plan.LegionNames: a name of Legion's it lacks is an
// error naming it.
func AppRoleFor(role claim.Role, deployed DeploymentApps, planned Planned) (AppRole, error) {
	app, inDeployment := deployed[role]
	switch plan.SourceOf(string(role), planned.Defines, inDeployment) {
	case plan.SourceLegion:
		switch role {
		case claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer, claim.RoleController, "acceptance":
			return Review, nil
		case claim.RoleImplementer, claim.RoleMerger:
			return Implement, nil
		}
		return "", fmt.Errorf("the role %s is Legion's, and AppRoleFor names no App for it", role)
	case plan.SourcePlan:
		if planned.Type == plan.TypeImplementation {
			return Implement, nil
		}
		return Review, nil
	case plan.SourceDeployment:
		return app, nil
	}
	return "", fmt.Errorf("the role %s is none of Legion's, and %w", role, ErrRoleNotDefined)
}

// RoleApps is what a reader of a claim's App holds: the deployment's roles, and the one read of
// a role of an issue's plan in force. The daemon builds the one value (internal/daemon's
// roleApps), whose Plan is the daemon's one reader of a role of a plan.
type RoleApps struct {
	Deployed DeploymentApps
	// Plan is what the plan in force of issue says of role; nil reads no plan, so a role of a plan
	// acts as no App.
	Plan func(ctx context.Context, issue string, role claim.Role) (Planned, error)
}

// Of is the App of role for a claim of issue: AppRoleFor, with the plan of issue read once for
// every role that is none of Legion's (plan.IsLegionName), a role of the deployment's too, since
// the plan wins over the deployment for a name it defines. A role of a plan is its issue's alone,
// since two issues can define one name with two types, so a reader names the claim's issue. A
// claim of no issue ("") reads no plan.
func (a RoleApps) Of(ctx context.Context, issue string, role claim.Role) (AppRole, error) {
	var planned Planned
	if a.Plan != nil && issue != "" && !plan.IsLegionName(string(role)) {
		var err error
		if planned, err = a.Plan(ctx, issue, role); err != nil {
			return "", fmt.Errorf("the plan of %s for the role %s: %w", issue, role, err)
		}
	}
	return AppRoleFor(role, a.Deployed, planned)
}
