package appauth

import (
	"errors"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// DeploymentApps is each role the deployment defines in its settings (`roles`) with the App its
// github_app names.
type DeploymentApps map[claim.Role]AppRole

// ErrRoleNotDefined is AppRoleFor's answer for a role that is none of Legion's and that the
// deployment does not define either: such a role acts as no App.
var ErrRoleNotDefined = errors.New("the deployment defines no such role")

// AppRoleFor is the one role-to-App decision. Keep this switch exhaustive over Legion's claim
// roles, plus the one non-claim role it also decides: the project controller, which acts as the
// review App (it writes no issue branch). Roles that can write an issue branch use implement;
// every verdict-only role, and the controller, uses review. The deployment's map never overrides
// one of these. Every other role acts as the App deployed names for it, and a role deployed does
// not name is ErrRoleNotDefined.
func AppRoleFor(role claim.Role, deployed DeploymentApps) (AppRole, error) {
	switch role {
	case claim.RoleArchitect, claim.RolePlanner, claim.RoleTester, claim.RoleReviewer, claim.RoleController:
		return Review, nil
	case claim.RoleImplementer, claim.RoleMerger:
		return Implement, nil
	}
	if app, ok := deployed[role]; ok {
		return app, nil
	}
	return "", fmt.Errorf("the role %s is none of Legion's, and %w", role, ErrRoleNotDefined)
}
