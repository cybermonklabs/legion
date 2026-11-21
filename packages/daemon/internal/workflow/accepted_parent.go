package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
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
// move, which the daemon records rather than undoes.
//
// It is the child-status kind every other word about a child entering or leaving its tree is
// told under, so it reaches the architect above the child by the route those already take.
func ChildUnderAcceptedParentNotice(child, ancestor string) texts.ChildStatusRecord {
	return texts.ChildStatusRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChildStatus, Issue: child}, Change: texts.ChangeBeneathAccepted,
		AcceptedAncestor: ancestor}
}
