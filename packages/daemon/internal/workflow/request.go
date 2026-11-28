package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// requestRefusal judges fact, where it is an architect's request on one issue
// (intake.ArchitectRequest), on the record and the Dispatch chain as the fact reads them, under
// the fact lock and before the fact's own handler reads anything: the one place every such fact
// passes (Apply). Its route judged the request on reads taken before that lock, and a change of
// parent in Dispatch, a new run or a new run of accepting can commit between the two. So the
// request stands only while its architect acts on the issue on the chain this transaction reads,
// by the rule its route judged (architectCallRefusal over the request's intake.ArchitectCall:
// ISSUE_NOT_OWNED, naming the architect claim that acts on the issue now, or the call's own
// code), while the issue is on the generation its route read (the request's StaleGeneration
// otherwise; a generation of 0 is a plan given for an issue the route read unrecorded, which
// planGiven judges), and, for a request about the run of accepting (intake.EnteredRequest), while
// that run stands (its StaleEntry otherwise). Apply leaves each refusal unspent, so the request
// made again is judged on the record and the chain as they stand then. An issue the record does
// not hold is the handler's to answer, and nil is answered for every fact that is no architect's
// request.
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
	if refusal, err := e.architectCallRefusal(ctx, tx, call.By, *issue, call.Call); err != nil || refusal != nil {
		return refusal, err
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
