package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closeChild closes, with the architect's reason, an issue under the caller's own issue on the
// Dispatch chain that the daemon holds no record of (intake.CloseChild): a child filed and never
// released, which no phase of Legion's ran and whose close nothing of the workflow follows (R24
// and R26 of dispatch://INFRA-3320). The close is one status write, done over the status the copy
// shows, queued in the fact's own transaction with the reason posted first.
//
// It is refused, in this order: on an unknown chain (CHAIN_UNKNOWN, R16); for an issue the daemon
// records, which ends through its workflow or a park (CHILD_RECORDED); for an issue that is not
// strictly under the caller's own on its chain (ISSUE_NOT_OWNED, R18); and while a Dispatch child
// of the issue is not done (CHILDREN_NOT_DONE, R24), since Dispatch closes an issue only once each
// of its children is done.
func (e *Engine) closeChild(ctx context.Context, tx pgx.Tx, fact intake.CloseChild) (intake.Result, error) {
	chains, err := record.ReadChains(ctx, tx, e.store, e.cfg.Project, []string{fact.Issue})
	if err != nil {
		return intake.Result{}, err
	}
	chain := chains.Chain(fact.Issue)
	if !chain.Known() {
		return intake.Result{Refusal: intake.ChainUnknown(fact.Issue, chain)}, nil
	}
	if recorded, err := e.store.Issue(ctx, tx, fact.Issue); err != nil || recorded != nil {
		if recorded != nil {
			return refused("CHILD_RECORDED", fmt.Sprintf("close_child of %s is refused: the daemon records %s (phase %s, status %s), so it ends through its workflow; park it with park_child "+parkRule,
				fact.Issue, fact.Issue, recorded.Phase, recorded.Status)), nil
		}
		return intake.Result{}, err
	}
	if !e.strictlyUnder(chain, fact.Architect) {
		return intake.Result{Refusal: &intake.Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf("close_child of %s is refused: on its Dispatch chain %s is not under the issue of %s, and close_child closes an issue under the caller's own",
			fact.Issue, fact.Issue, fact.Architect)}}, nil
	}
	if open := childrenNotDone(chains, fact.Issue); len(open) > 0 {
		return childrenNotDoneRefusal("close_child", fact.Issue, open), nil
	}
	status, _ := chains.Status(fact.Issue)
	return intake.Result{}, e.enqueue(ctx, tx, fact.Issue, record.CloseStatusWrite("done", status, fact.Message(), 0))
}

// strictlyUnder says whether architect is the architect claim of an issue above the chain's own
// issue: one of its links, never the issue itself.
func (e *Engine) strictlyUnder(chain record.Chain, architect claim.Token) bool {
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil || architect == "" {
		return false
	}
	for _, link := range chain.Links {
		if token, err := claim.NewToken(project, link, claim.RoleArchitect); err == nil && token == architect {
			return true
		}
	}
	return false
}
