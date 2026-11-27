package docs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// decisionType is the block type whose server-owned attributes settlement holds to the store.
const decisionType = "decision"

// decisionServerAttributes is the names of the decision type's server-owned attributes, read off
// the installed manifest once when it was compiled (pmdoc.ServerAttributes), as the ask type's are
// (pmdoc.ServerAttributesInMarkdown), and none where the installed manifest declares no decision
// type. Every settlement shares the list, which none writes; the store keeps each attribute's
// value or relation under its store name (schemanodes.StoreName).
func decisionServerAttributes() []string {
	return pmdoc.ServerAttributes(decisionType)
}

// reconcileDecisionBlocks holds the server-owned attributes of every decision block of tree, the
// tree settlement read of document artifactID, to what the store holds of the block's node: an
// `actor` attribute (decided_by, kept_by) to the far end of the node's current tuple of that
// kind, and any other (kept_text_sum) to the node's current value, each under its store name
// (schemanodes.StoreName). A markdown write never carries one of them (pmdoc.StripServerOwnedAttrs
// clears them at every agent boundary), but a browser's editor can put any value on a decision in
// the live tree, and every other editor of the document would then read an author or a Keep the
// store never recorded. So settlement overwrites a value the store does not hold with the one it
// does, and takes out one the store holds none of; the store's own record is never moved by
// what the tree says (refs.DecisionFacts). An attribute the block does not carry is left absent,
// so a document written through markdown is not rewritten by every settlement that reads it.
//
// Each change is a repair (settlementReconciliation.repair), written into the live document with
// the ask blocks' (repairLive); it emits no event, since it restores what the store already says.
func (r *settlementReconciliation) reconcileDecisionBlocks(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error {
	server := decisionServerAttributes()
	if len(server) == 0 {
		return nil
	}
	type claimed struct {
		node *pmdoc.Node
		id   string
	}
	var decisions []claimed
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != decisionType {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if _, repeated := seen[id]; id == "" || repeated {
			return true
		}
		seen[id] = struct{}{}
		for _, name := range server {
			if value, carried := node.Attrs[name]; carried && value != nil {
				decisions = append(decisions, claimed{node: node, id: id})
				break
			}
		}
		return true
	})
	if len(decisions) == 0 {
		return nil
	}
	var refKey string
	if err := tx.QueryRow(ctx, `select ref_key from artifacts where id = $1`, artifactID).Scan(&refKey); err != nil {
		return fmt.Errorf("read the reference of document %s: %w", artifactID, err)
	}
	addresses := make([]string, len(decisions))
	for index, decision := range decisions {
		addresses[index] = refs.BlockAddress(refKey, decision.id)
	}
	kinds := make([]string, len(server))
	for index, name := range server {
		kinds[index] = schemanodes.StoreName(decisionType, name)
	}
	// The newest tuple of each kind each node holds (graphstore.CurrentTupleRead): a relation of
	// these kinds holds one far end at a time (decided_by is set once, a Keep names one person), and
	// a value is its newest.
	rows, err := tx.Query(ctx, graphstore.CurrentTupleRead(`t.from_id, t.kind`, `t.from_id, t.kind, t.to_id, t.asserted`,
		`t.from_kind = $1 and t.from_id = any($2::text[]) and t.kind = any($3::text[])`), refs.BlockNodeKind, addresses, kinds)
	if err != nil {
		return fmt.Errorf("read the server attributes the store holds of the decisions of %s: %w", artifactID, err)
	}
	held := map[[2]string]string{}
	for rows.Next() {
		var address, kind, value string
		var asserted bool
		if err := rows.Scan(&address, &kind, &value, &asserted); err != nil {
			rows.Close()
			return fmt.Errorf("scan a server attribute of a decision of %s: %w", artifactID, err)
		}
		if asserted {
			held[[2]string{address, kind}] = value
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the server attributes the store holds of the decisions of %s: %w", artifactID, err)
	}
	for index, decision := range decisions {
		want := map[string]string{}
		for at, name := range server {
			if value, stored := held[[2]string{addresses[index], kinds[at]}]; stored {
				want[name] = value
			}
		}
		r.repair(decision.node, decision.id, func(node *pmdoc.Node) bool {
			changed := false
			for _, name := range server {
				carried, present := node.Attrs[name]
				if !present || carried == nil {
					continue
				}
				stored, holds := want[name]
				switch {
				case !holds:
					delete(node.Attrs, name)
					changed = true
				case carried != stored:
					node.Attrs[name] = stored
					changed = true
				}
			}
			return changed
		})
	}
	return nil
}
