package graphstore

import (
	"context"
	"fmt"
	"strings"
)

// TupleMovedTo points from a block address that no longer stands to the block it moved to: a
// move of a block from one document to another is a delete and an
// insert, which changes the block's address, so the move appends one of these, from the old
// address to the new one, with the time of the move, and rewrites no stored pointer. The restore
// and the conversion of that issue write it; a later change moves blocks through it again.
// The conversion also points a section's heading at an ask node, where the section cites an
// answered ask a person removed from its spec: the chain ends at that ask. The pre-mortem
// conversion (api.ConvertPremortem) appends one from the address of the document it retires -
// kind artifact, the document's ref_key - to the lens it wrote in the document's place. No table
// projects it.
const TupleMovedTo = "moved_to"

// MovedToHops is how many moved_to tuples a reader follows from one address before it reads the
// address as resolving to no block. moved_to_reach, the SQL function every resolver reads a move
// through (migration 20261117002006), holds the same bound in its loop, and the census follows a
// far end no further (graphcensus' storeOnlyRead).
const MovedToHops = 8

// Moved is where the moved_to tuples from an address reach (MovedTo): a block a document_blocks
// row holds, an ask whose row stands, or, from a document's address, a lens that is not gone.
// Address is how a reader names it: the block's node address, the ask's own address,
// `dispatch://KEY/ask/<id>` for an ask of an issue and `dispatch://PROJECT/artifact/<slug>/ask/<id>`
// for one of a project document, as refs.AskRef writes it ("" for an ask of an agent's
// conversation, which no address names), or the lens's, `dispatch://lens/<uuid>`. Ask is the ask's
// id where the chain ends at an ask, Lens the lens's uuid where it ends at a lens, and each is ""
// otherwise.
type Moved struct {
	Address, Ask, Lens string
}

// MovedTo answers what the moved_to tuples from address reach, and true: address itself when a
// document_blocks row holds it, else the far end of the newest moved_to that address states now,
// read through graph_tuples_as_of_out as AsOfOut reads a relation, and so on from there, each step
// leaving an address no row holds, until a block that stands or an ask whose row stands. It
// answers false where the chain stops before either: an address that states no moved_to, a far
// end that is neither, a cycle, or a chain longer than MovedToHops.
//
// From a document's address, its ref_key, which holds no '#' where a block's address always
// holds one, it answers the lens the newest moved_to that address states points at, and true,
// where that lens is not gone: the lens the pre-mortem conversion retired the document into
// (migration 20261117042519, which block_moved_to reads it through). It answers false for a
// document's address that states no such moved_to, and for one whose lens is gone.
//
// The rule is moved_to_reach's, and block_moved_to's for a document's address, called here alone,
// so the reading of api's blockReferenceNode and getReferences and the readings refs.Closure and
// refs.BlockLinks make inside their own statements are one rule.
func MovedTo(ctx context.Context, q Queryer, address string) (Moved, bool, error) {
	if !strings.Contains(address, "#") {
		var lens *string
		if err := q.QueryRow(ctx, `select block_moved_to($1)`, address).Scan(&lens); err != nil {
			return Moved{}, false, fmt.Errorf("follow the moves of %s: %w", address, err)
		}
		if lens == nil {
			return Moved{}, false, nil
		}
		return Moved{Address: "dispatch://" + KindLens + "/" + *lens, Lens: *lens}, true, nil
	}
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
