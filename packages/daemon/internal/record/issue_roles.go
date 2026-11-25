package record

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// IssueRoles is every role whose claim issue may hold: Legion's six (claim.Roles), whether or not
// each ever ran, then each role the deployment defines that issue's phases rows name, in the order
// the rows come (by role). A start raises an entry on its role's row (workflow's enter), so a role
// that ran on the issue has a row, and the row stays when a later version of the plan drops the
// task that named the role. It is the one reading of an issue's roles that a close, a linger's
// end, the stop of a carried child's old tree and the removal of a workspace walk, so a claim of a
// role of the deployment is reached wherever one of Legion's is.
func IssueRoles(ctx context.Context, tx pgx.Tx, store Store, issue string) ([]claim.Role, error) {
	rows, err := store.Phases(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	roles := append([]claim.Role{}, claim.Roles...)
	for _, row := range rows {
		if claim.IsDeploymentRole(row.Role) {
			roles = append(roles, row.Role)
		}
	}
	return roles, nil
}
