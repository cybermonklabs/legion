package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/stateevent"
	"github.com/sjawhar/legion/daemon/internal/store/precommit"
)

// An issue's `tree` on the state route is the tree its Dispatch chain names (Chains.StateTree, R22
// of dispatch://INFRA-3320), read over every record of the project (ObservedTrees), so no one row
// decides it: a write of one issue's record or of one row of the Dispatch copy can move the tree of
// every issue under it. PutIssue's own comparison of the row it replaces (ObservationOf) therefore
// names no tree. The tree's state events come from this file instead. A writer of what a tree is
// read from (watchTrees's callers: PutIssue where it moves an issue into or out of Legion's work,
// records it first or marks it holding its own gate, PutDispatchIssue where it writes a row of
// the copy or moves its parent, TakeDispatchRefill where it empties the copy) reads every tree of
// the project before its first such write in the transaction, and the transaction's end
// (package precommit) reads them again and queues one state_change naming `tree` for each issue
// whose tree moved, in key order, in the same transaction, after every other row the transaction
// queued. A transaction that moves nothing queues nothing, and one that rolls back queues nothing,
// since its rows roll back with it. Nothing is stored between transactions: each transaction
// compares the trees as they stood at its own first such write with the trees it leaves.
//
// The writers named are every writer of a column or a row StateTree is read from: an issue's
// phase and status (InLegionsWork) and its mark that it holds its own gate (Issue.HoldsGate) on
// issues, and a row and its parent on dispatch_issues (Chain). MarkHoldsGate marks only issues the
// pass's read finds roots (Chains.IsRoot), which hold their own gate already, so it moves no tree
// and reads nothing here.

// treeWatch names the one settle of one project's trees in one transaction (precommit.Once).
type treeWatch struct {
	store   *Postgres
	project string
}

// ObservedTrees is the tree the state route serves for each issue project records
// (projection.Project and projection.Observe): Chains.StateTree over the chains ReadChains reads
// with every record of project among its keys, as record.ReadLineFacts reads them for the route.
func ObservedTrees(ctx context.Context, tx pgx.Tx, store Store, project string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `select key from issues where project = $1 order by key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the records of %s for their trees: %w", project, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the records of %s for their trees: %w", project, err)
	}
	chains, err := ReadChains(ctx, tx, store, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the trees of %s: %w", project, err)
	}
	trees := make(map[string]string, len(keys))
	for _, key := range keys {
		trees[key] = chains.StateTree(key)
	}
	return trees, nil
}

// watchTrees reads project's trees (ObservedTrees) before the first write of the transaction
// that can move one, and registers the transaction's end to compare them with the trees it
// leaves (settleTrees). With the state events off, a later write of the same transaction, and a
// transaction that runs no settles (a write made outside store.TransactWith, which no daemon
// transaction is) read nothing.
func (s *Postgres) watchTrees(ctx context.Context, tx pgx.Tx, project string) error {
	state := s.state.Load()
	if state == nil {
		return nil
	}
	watch := treeWatch{store: s, project: project}
	if scoped, registered := precommit.Scoped(ctx, watch); !scoped || registered {
		return nil
	}
	before, err := ObservedTrees(ctx, tx, s, project)
	if err != nil {
		return err
	}
	precommit.Once(ctx, watch, func(ctx context.Context, tx pgx.Tx) error {
		return s.settleTrees(ctx, tx, state, project, before)
	})
	return nil
}

// settleTrees queues, in tx, one state change naming `tree` for each issue of project whose tree
// the transaction moved from before, the trees read before its first such write: its old tree,
// null for an issue the transaction recorded first, and its new one, in key order. The rows are
// due at the transaction's own time, as PutIssue's are.
func (s *Postgres) settleTrees(ctx context.Context, tx pgx.Tx, state *statePublication, project string, before map[string]string) error {
	after, err := ObservedTrees(ctx, tx, s, project)
	if err != nil {
		return err
	}
	var due time.Time
	for _, key := range slices.Sorted(maps.Keys(after)) {
		tree := after[key]
		old, known := before[key]
		if known && old == tree {
			continue
		}
		if due.IsZero() {
			if err := tx.QueryRow(ctx, `select now()`).Scan(&due); err != nil {
				return fmt.Errorf("read the time of the tree changes of %s: %w", project, err)
			}
		}
		change := StateFieldChange{Field: StateFieldTree, Old: stateevent.Null}
		if known {
			if change.Old, err = json.Marshal(old); err != nil {
				return err
			}
		}
		if change.New, err = json.Marshal(tree); err != nil {
			return err
		}
		if err := s.queueStateChange(ctx, tx, state, project, key, []StateFieldChange{change}, due); err != nil {
			return err
		}
	}
	return nil
}

// issueMovesTrees says whether writing issue can move a tree (StateTree): a record written first
// (first: PutIssue's locked read found no row of it), a write that moves it into or out of
// Legion's work (InLegionsWork, its phase and its status), and one that marks it holding its own
// gate (Issue.HoldsGate, which PutIssue writes set-only). No other column of issues is read for
// a tree, so every other write of the record moves none.
func issueMovesTrees(ctx context.Context, tx pgx.Tx, first bool, issue Issue) (bool, error) {
	if first {
		return true, nil
	}
	var stored Issue
	var phaseValue string
	if err := tx.QueryRow(ctx, `select phase, status, holds_gate from issues where key = $1`, issue.Key).Scan(
		&phaseValue, &stored.Status, &stored.HoldsGate); err != nil {
		return false, fmt.Errorf("read what the trees read of the record: %w", err)
	}
	stored.Phase = phase.Phase(phaseValue)
	return InLegionsWork(stored) != InLegionsWork(issue) || (issue.HoldsGate && !stored.HoldsGate), nil
}

// copyRowMovesTrees says whether reading, a reading PutDispatchIssue writes, can move a tree
// (StateTree): a row the copy does not hold yet, and a parent the reading carries that is not the
// one the row holds. A chain reads no status, blocker, label or rank of a row. A reading whose
// parent a newer value of the row holds back reads here as a move all the same, and the
// comparison at the transaction's end then queues nothing for it.
func copyRowMovesTrees(ctx context.Context, tx pgx.Tx, reading DispatchReading) (bool, error) {
	var parent *string
	err := tx.QueryRow(ctx, `select parent from dispatch_issues where key = $1`, reading.Key).Scan(&parent)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the parent the copy holds: %w", err)
	}
	if !reading.Parent.Carried {
		return false, nil
	}
	held, wanted := "", ""
	if parent != nil {
		held = *parent
	}
	if next := ParentOf(reading.Parent.Value); next != nil {
		wanted = *next
	}
	return held != wanted, nil
}
