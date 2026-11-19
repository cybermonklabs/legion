package graphstore

import (
	"context"
	"fmt"
)

// TupleMovedTo points from a block address that no longer stands to the block it moved to: a
// move of a block from one document to another is a delete and an
// insert, which changes the block's address, so the move appends one of these, from the old
// address to the new one, with the time of the move, and rewrites no stored pointer. The restore
// and the conversion of that issue write it; a later change moves blocks through it again.
// The pre-mortem conversion (api.ConvertPremortem) appends one from the address of the document it
// retires - kind artifact, the document's ref_key - to the lens it wrote in the document's place.
// The conversion also points a section's heading at an ask node, where the section cites an
// answered ask a person removed from its spec: the chain ends at that ask. No table projects it.
const TupleMovedTo = "moved_to"

// MovedToHops is how many moved_to tuples a reader follows from one address before it reads the
// address as resolving to no block. block_address_walk, the SQL function every resolver walks an
// address's moves through (migration 20261118165901, which moved_to_reach and block_moved_to read
// through), holds the same bound in its loop, and the census follows a far end no further
// (graphcensus' storeOnlyRead).
const MovedToHops = 8

// Moved is where the moved_to tuples from an address reach (MovedTo): a block a document_blocks
// row holds, an ask whose row stands, or, from a document's address, the lens it was retired into.
// Address is how a reader names it: the block's node address, the ask's own address,
// `dispatch://KEY/ask/<id>` for an ask of an issue and `dispatch://PROJECT/artifact/<slug>/ask/<id>`
// for one of a project document, as refs.AskRef writes it ("" for an ask of an agent's
// conversation, which no address names), or the lens's id. Ask is the ask's id where the chain
// ends at an ask, and "" where it reaches a block or a lens.
type Moved struct {
	Address, Ask string
}

// MovedTo answers what address resolves to, and true. A block's address resolves to the block a
// document_blocks row holds in one order (T17 of this branch's plan): the far end of the newest
// moved_to that address states now, read as AsOfOut reads a relation, and so on from there while
// another current moved_to stands at the far end; then, where no tuple stands, the same block id
// in the room the reached address's document is edited in (its document_rooms row: the node
// `<tree ref_key>#<block id>` of an issue room); then the reached address itself. Where that
// reaches no block, the reached address's newest moved_to may end at an ask whose row stands
// instead: the heading of a section that cites an answered ask a person removed. It answers false
// where none of these is held, and where the walk stops early: a far end that is neither a block
// nor an ask, a cycle, or a chain longer than MovedToHops.
//
// From a document's address, its ref_key, which holds no '#' where a block's address always
// holds one, it answers the id of the lens the newest moved_to that address states points at, and
// true, where that lens is not gone: the lens the pre-mortem conversion retired the document into
// (migration 20261117042519). It answers false for a document's address that states no such
// moved_to, and for one whose lens is gone.
//
// The rule is moved_to_reach's (migration 20261118165901), called here alone, which block_moved_to
// answers the block or lens of, so the reading of api's blockReferenceNode and the readings
// refs.Closure, refs.BlockLinks, refs.BlockTargets and the block index's pointer writer make inside
// their own statements (refs.ResolvedBlockSQL, refs' movedTarget) are one rule.
func MovedTo(ctx context.Context, q Queryer, address string) (Moved, bool, error) {
	var kind, id, issue, project, slug *string
	// The ask's row is read by its key only where the chain ended at an ask: a block's address is
	// no uuid, and the case keeps it from being cast as one.
	if err := q.QueryRow(ctx, `
		select reach.kind, reach.id, k.issue_key, a.project_key, a.slug
		from moved_to_reach($1) reach
		left join asks k on k.id = case when reach.kind = '`+KindAsk+`' then reach.id::uuid end
		left join artifacts a on a.id = k.block_artifact_id
	`, address).Scan(&kind, &id, &issue, &project, &slug); err != nil {
		return Moved{}, false, fmt.Errorf("follow the moves of %s: %w", address, err)
	}
	switch {
	case kind == nil || id == nil:
		return Moved{}, false, nil
	case *kind != KindAsk:
		return Moved{Address: *id}, true, nil
	case issue != nil:
		return Moved{Address: "dispatch://" + *issue + "/ask/" + *id, Ask: *id}, true, nil
	case project != nil && slug != nil && *project != "" && *slug != "":
		return Moved{Address: "dispatch://" + *project + "/artifact/" + *slug + "/ask/" + *id, Ask: *id}, true, nil
	}
	return Moved{Ask: *id}, true, nil
}
