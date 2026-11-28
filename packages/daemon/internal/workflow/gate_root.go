package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The workflow's readers of an issue's gate root read it on the Dispatch chain this transaction
// reads (record.Chains, R15 of dispatch://INFRA-3320), never the tree the issue's record stores:
// a change of parent moves them with the chain.

// gateRootOf is the record of issue's gate root on the Dispatch chain this transaction reads
// (record.Chains.StateTree, R15 and R22): the nearest issue at or above it in Legion's work that
// holds its own gate, or, for an issue not in Legion's work, the nearest issue above it that the
// daemon records and that holds its own gate, else issue itself. It is what a decision about
// issue's run reads as the tree it runs in, and a question of issue alone, so it reads issue's
// own chain (record.ReadChainsOf).
func (e *Engine) gateRootOf(ctx context.Context, tx pgx.Tx, issue record.Issue) (record.Issue, error) {
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return record.Issue{}, err
	}
	if root, recorded := chains.Record(chains.StateTree(issue.Key)); recorded {
		return root, nil
	}
	return issue, nil
}

// gateHolder is the record of key where it holds its own design gate on the Dispatch chain
// (record.Chains.HoldsOwnGate), with the chains that read it, and nil otherwise: the issue a reader
// of a root's own affairs (its asks, its gate, its architect's start) acts for. The chains are the
// whole project's (record.ReadChains), since a caller asks of them who stands in that root's tree
// (sessionOfGateTree).
func (e *Engine) gateHolder(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, *record.Chains, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, nil, err
	}
	chains, err := record.ReadChains(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil || !chains.HoldsOwnGate(issue.Key) {
		return nil, nil, err
	}
	return issue, chains, nil
}

// holdsOwnGate says whether issue holds its own design gate on the Dispatch chain this transaction
// reads (record.Chains.HoldsOwnGate): a root, or an issue that ran as one and keeps its gate under
// a parent in Legion's work. It is a question of issue alone, so it reads issue's own chain
// (record.ReadChainsOf).
func (e *Engine) holdsOwnGate(ctx context.Context, tx pgx.Tx, issue record.Issue) (bool, error) {
	chains, err := record.ReadChainsOf(ctx, tx, e.store, issue.Project, []string{issue.Key})
	if err != nil {
		return false, err
	}
	return chains.HoldsOwnGate(issue.Key), nil
}

// sessionOfGateTree says whether the agent session holds a claim of root, a gate root, or of an
// issue whose gate root it is on chains (record.Chains.GateTree): a session of root's tree
// (record.Store.SessionClaimsOf).
func (e *Engine) sessionOfGateTree(ctx context.Context, tx pgx.Tx, chains *record.Chains, root, session string) (bool, error) {
	return e.store.SessionClaimsOf(ctx, tx, chains.GateTree(root), session)
}
