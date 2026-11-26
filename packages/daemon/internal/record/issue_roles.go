package record

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// IssueRoles is every role whose claim issue may hold: Legion's six (claim.Roles), whether or not
// each ever ran, then each role the deployment defines that issue's phases rows name, once, in the
// order the rows come (by role). A start raises an entry on its role's row (workflow's enter), so a
// role that ran on the issue has a row, one for each step it ran where it stands at two steps of a
// task, and the row stays when a later version of the plan drops the task that named the role. It
// is the one reading of an issue's roles, which workflow's claimTargets takes beside the issue's
// stored claims (Store.IssueClaims) for a close, a linger and its end, and the stop of a carried
// child's old tree: a row names each stored claim, and a row by role reaches each role whose worker
// claim is none of those, so a claim of a role of the deployment is reached wherever one of
// Legion's is, and once.
func IssueRoles(ctx context.Context, tx pgx.Tx, store Store, issue string) ([]claim.Role, error) {
	rows, err := store.Phases(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	roles := append([]claim.Role{}, claim.Roles...)
	for _, row := range rows {
		if claim.IsDeploymentRole(row.Role) && !slices.Contains(roles, row.Role) {
			roles = append(roles, row.Role)
		}
	}
	return roles, nil
}
