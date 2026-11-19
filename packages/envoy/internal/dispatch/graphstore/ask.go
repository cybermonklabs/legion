package graphstore

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The relations and values an ask's node holds beside its base attributes. The three relations
// to artifacts are what the asks row's block_artifact_id, anchor and
// artifact_id say of the ask: the document holding its block or the document its anchor quotes
// (anchored_to, TupleAnchoredTo), the document an ask on no issue belongs to (owned_by,
// TupleOwnedBy), and the document holding its block (part_of, TuplePartOf), which is where a copy
// of the ask reads its source's document from.
//
// The answer is held on the node and nowhere else: the asks row's state, answer and resolution
// are the projection of these, written by asks.Settle in the transaction that appends them. The
// answer state is `answer_state`, never `state`, which is the node's own live or gone
// (schemanodes.StoreName). answered_by and decided_by point at the person who answered.
// followed_by points at each session that follows the ask (asks.Follow), and ask_followers is its
// projection.
const (
	TupleAnswerState = "answer_state"
	TupleAnswer      = "answer"
	TupleSelected    = "selected"
	TupleAnsweredAt  = "answered_at"
	TupleResolution  = "resolution"
	TupleAnsweredBy  = "answered_by"
	TupleDecidedBy   = "decided_by"
	TupleFollowedBy  = "followed_by"
	// TupleCopiedFrom points from the block node of a copy of an ask, on another document of its
	// owner, to the ask it was copied from (docs' copiedAskSources). No table projects it.
	TupleCopiedFrom = "copied_from"
	// TupleRemovedBy points from an answered ask's node to the person who took its block out of
	// the document its row names: that person's removal stays, and the decisions step never
	// writes the block back. It is retracted once an ask block of that ask stands in that document
	// again. No table projects it.
	TupleRemovedBy = "removed_by"
)

// AskPlaces are the addresses (artifacts.ref_key) of the documents an ask's node relates to, each
// empty where the ask has none: Anchor is the document holding its block, else the document its
// anchor quotes, which graph_edges' old arm reads as coalesce(block_artifact_id, anchor's
// artifact_id); Owner is the document an ask on no issue belongs to (artifact_id); Document is the
// document holding its block (block_artifact_id).
type AskPlaces struct {
	Anchor, Owner, Document string
}

// Fit is whether every address of p is inside the store's bound (AddressFits). An ask any of whose
// relation ends is past it is left out of the store's relations whole, as a comment is (api's
// commentStored): its node is written, but none of its three relations, and its row names no node,
// so graph_edges' old arms answer every edge of it rather than the tuple arm answering it short one.
func (p AskPlaces) Fit() bool {
	return AddressFits(p.Anchor) && AddressFits(p.Owner) && AddressFits(p.Document)
}

// ReadAskPlaces reads the addresses of the documents an ask relates to, in one statement in the
// caller's transaction: anchor is the artifact id its block's document or its anchor names, owner
// the artifact id an ask on no issue belongs to, and block the artifact id holding its block, each
// nil where the ask has none. An id no row answers reads as no address.
func ReadAskPlaces(ctx context.Context, q Queryer, anchor, owner, block *string) (AskPlaces, error) {
	var places AskPlaces
	if anchor == nil && owner == nil && block == nil {
		return places, nil
	}
	if err := q.QueryRow(ctx, `
		select coalesce((select ref_key from artifacts where id = $1::uuid), ''),
		       coalesce((select ref_key from artifacts where id = $2::uuid), ''),
		       coalesce((select ref_key from artifacts where id = $3::uuid), '')
	`, anchor, owner, block).Scan(&places.Anchor, &places.Owner, &places.Document); err != nil {
		return AskPlaces{}, fmt.Errorf("read the documents of an ask: %w", err)
	}
	return places, nil
}

// AskFacts is what this store holds of one ask when it is created, beside its node's base
// attributes: its node (AskNode), its three relations to documents where it has them (AskPlaces),
// and who asked it (AuthorFacts: the authored_by tuple, the author's node, and the aliases the
// server proved) where the actor names a person or a session. Every node comes before any tuple a
// writer appends, the ask's node first. marked is whether the asks row names the node: false for
// an ask any of whose relation ends is past the store's bound (AskPlaces.Fit), whose relations are
// left out and whose row the old arms of graph_edges answer.
//
// It is the one statement of an ask's creation, which the three create sites write
// (api's createAskFor and openApprovalAsk, docs' createAskBlock) and the boot step's backfill
// states for a row written before. at is the asks row's created_at, so each relation is dated as
// graph_edges' old arm dated it, and event the event that recorded the ask, or NoEvent for a write
// held for its cause (WriteUncaused).
//
// A later move of the ask - which the restore alone makes - states the three relations as
// Claims through WriteArms instead (AskClaims), so the far ends it leaves are retracted.
func AskFacts(id, project string, parties []string, author model.Actor, at time.Time, event int64, places AskPlaces) (nodes []Node, tuples []Tuple, marked bool) {
	node := AskNode(id, project, parties, author, at)
	node.SourceEvent = event
	nodes = []Node{node}
	if marked = places.Fit(); marked {
		for _, relation := range askRelations(places) {
			if relation.address == "" {
				continue
			}
			tuples = append(tuples, Tuple{
				FromKind: KindAsk, FromID: id, Kind: relation.kind,
				ToKind: KindArtifact, ToID: relation.address,
				At: at, Who: author, SourceEvent: event, Asserted: true,
			})
		}
	}
	if authors, facts, named := AuthorFacts(KindAsk, id, author, at, event); named {
		nodes = append(nodes, authors...)
		tuples = append(tuples, facts...)
	}
	return nodes, tuples, marked
}

// AskClaims is the three relations of an ask's node as Claims, for the write that moves an ask
// from where its row stood (the restore): each claim retracts the far end the node held under
// that kind and asserts the new one, and a relation the ask no longer has retracts what it held.
func AskClaims(id string, places AskPlaces, who model.Actor, at time.Time, event int64) []Claim {
	relations := askRelations(places)
	claims := make([]Claim, 0, len(relations))
	for _, relation := range relations {
		claims = append(claims, Claim{
			FromKind: KindAsk, FromID: id, Kind: relation.kind,
			ToKind: KindArtifact, ToID: relation.address,
			At: at, Who: who, SourceEvent: event,
		})
	}
	return claims
}

// askRelations is the three relations of places in the order every writer states them.
func askRelations(places AskPlaces) [3]struct{ kind, address string } {
	return [3]struct{ kind, address string }{
		{TupleAnchoredTo, places.Anchor}, {TupleOwnedBy, places.Owner}, {TuplePartOf, places.Document},
	}
}

// AskRowNode is the node columns an asks or ask_followers row names: the ask kind and the ask's
// own id where marked, and null and null where it is not, whose row the old arms answer.
func AskRowNode(id string, marked bool) (nodeKind, nodeAddress *string) {
	if !marked {
		return nil, nil
	}
	kind := KindAsk
	return &kind, &id
}
