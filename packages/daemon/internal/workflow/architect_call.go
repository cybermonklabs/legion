package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// architectCallRefusal is the check of an architect's call where its fact commits (decision 5 of
// dispatch://INFRA-4730): the API judged the call before it built the fact, in a transaction of
// its own, and a change of parent can land between the two, so the workflow reads the Dispatch
// chain of issue again in the fact's own transaction (record.ReadChainsOf over issue's own chain,
// all the rule asks of: it refuses an unknown chain first) and refuses the call the
// chain no longer allows (intake.CallRefusal), with the code that names the architect who may act.
// architect is the caller's architect claim token the fact carries, "" for a fact no architect
// sent (a phase worker's own backward move, the operator's retry, a registration the workflow
// makes itself), which is held to nothing here but close_root's root.
func (e *Engine) architectCallRefusal(ctx context.Context, tx pgx.Tx, architect claim.Token, issue record.Issue, call intake.ArchitectCall) (*intake.Refusal, error) {
	if architect == "" && !call.Roots() {
		return nil, nil
	}
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return nil, err
	}
	return intake.CallRefusal(ctx, tx, e.store, issue.Project, chains, architect, "", issue.Key, call)
}
