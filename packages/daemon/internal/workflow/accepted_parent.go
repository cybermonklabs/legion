package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// openBeneath is OpenBeneath over the tree issue belongs to, read here.
func (e *Engine) openBeneath(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]string, error) {
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	return record.OpenBeneath(issue, members), nil
}

// ChildUnderAcceptedParentNotice is what the architect above the child is told when a person
// moves an issue to todo beneath an ancestor whose own reviewed head the daemon has already
// accepted (record.AcceptedAncestor): the issue is recorded, and left standing at admitted until
// that ancestor closes, because work entering the workflow beneath it now would let that ancestor
// merge a head which does not carry the work. The two doors an agent uses refuse such an issue
// outright (the API's CHILD_UNDER_ACCEPTED_PARENT), so what reaches this is a person's own
// move, which the daemon records rather than undoes. The backward move it offers is refused once
// the ancestor's pull request has merged (record.Accepted holds merging, and merged work never
// moves back), so the notice says so.
//
// It is the child-status kind every other word about a child entering or leaving its tree is
// told under, so it reaches the architect above the child by the route those already take.
func ChildUnderAcceptedParentNotice(child, ancestor string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"%[1]s is beneath %[2]s, whose own reviewed head the daemon has already accepted, so %[1]s is recorded at admitted and nothing starts it until %[2]s closes. "+
			"Take %[2]s back with a backward move to run %[1]s under it now, unless the pull request of %[2]s has merged, or leave %[1]s where it is and it starts once %[2]s has closed.",
		child, ancestor)}
}
