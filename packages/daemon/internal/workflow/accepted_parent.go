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
// moves an issue to todo beneath an ancestor whose head the daemon has already accepted
// (record.AcceptedAncestor): the head the last step of the ancestor's task passed, which is the
// reviewed head only where that task has a reviewer step. The issue is recorded, and left
// standing at admitted while that ancestor stands in retro or merging, because work entering the
// workflow beneath it now would let that ancestor close without the work: by merging a head that
// does not carry it, or at the end of its retro where it has no pull request. The two doors an
// agent uses refuse such an issue outright, for the same reason (the API's
// CHILD_UNDER_ACCEPTED_PARENT), so what reaches this is a person's own move, which the daemon
// records rather than undoes.
//
// The ancestor stands in retro or merging (record.Accepted), where a backward move is taken only
// from the role that works it there (backwardRefusal): the agent of its retro, which is its own
// architect where the retro is the owner's, or its merger. So the notice names who can take it
// back.
//
// It is the child-status kind every other word about a child entering or leaving its tree is
// told under, so it reaches the architect above the child by the route those already take.
func ChildUnderAcceptedParentNotice(child, ancestor string) record.Notice {
	return record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf(
		"%[1]s is beneath %[2]s, whose head the last step of its task passed and the daemon has already accepted, so %[1]s is recorded at admitted and nothing starts it while %[2]s stands in retro or merging. "+
			"To run %[1]s under it now, %[2]s must go back to an earlier step: a request_backward_move by the agent that works it now, which is the agent of its retro (its own architect where the retro is the owner's) or its merger. "+
			"Or leave %[1]s where it is, and it starts once %[2]s has closed.",
		child, ancestor)}
}
