package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// requestRefusal judges fact, where it is an architect's request on one issue
// (intake.ArchitectRequest), on the record as the fact reads it, under the fact lock and before
// the fact's own handler reads anything: the one place every such fact passes (Apply). Its route
// judged the request on a read of the record taken before that lock, and an adoption, a new run,
// a re-parent or a new run of accepting can commit between the two. So the request stands only
// while its architect owns the issue now by the rule its route judged — an architect of the
// issue's tree (record.ArchitectOfTree, ISSUE_OUTSIDE_TREE otherwise), and for OwnerAbove the
// issue's own architect or one above it (record.ArchitectAbove, ISSUE_NOT_OWNED otherwise) — while
// the issue is on the generation its route read (the request's StaleGeneration otherwise; a
// generation of 0 is a plan given for an issue the route read unrecorded, which planGiven judges),
// and, for a request about the run of accepting (intake.EnteredRequest), while that run stands
// (its StaleEntry otherwise). Apply leaves each refusal unspent, so the request made again is
// judged on the record as it stands then. An issue the record does not hold is the handler's to
// answer, and nil is answered for every fact that is no architect's request.
func (e *Engine) requestRefusal(ctx context.Context, tx pgx.Tx, fact intake.Fact) (*intake.Refusal, error) {
	request, ok := fact.(intake.ArchitectRequest)
	if !ok {
		return nil, nil
	}
	call, ok := request.Request()
	if !ok {
		return nil, nil
	}
	issue, err := e.store.Issue(ctx, tx, call.Issue)
	if err != nil || issue == nil {
		return nil, err
	}
	owned, err := record.ArchitectOfTree(ctx, e.store, tx, call.By, *issue)
	if err != nil {
		return nil, err
	}
	if !owned {
		return intake.IssueOutsideTree(issue.Key, call.By, call.Operation), nil
	}
	if call.Owners == record.OwnerAbove {
		above, err := record.ArchitectAbove(ctx, e.store, tx, call.By, *issue)
		if err != nil {
			return nil, err
		}
		if !above {
			return intake.IssueNotOwned(call.Operation, issue.Key, call.By), nil
		}
	}
	if call.Generation != 0 && call.Generation != issue.Generation {
		return request.StaleGeneration(issue.Generation), nil
	}
	entered, ok := fact.(intake.EnteredRequest)
	if !ok {
		return nil, nil
	}
	now, err := e.store.OpenStep(ctx, tx, issue.Key)
	if err != nil {
		return nil, err
	}
	if now != entered.ReadEntry() {
		return entered.StaleEntry(now), nil
	}
	return nil, nil
}
