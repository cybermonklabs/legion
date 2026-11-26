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
	// TupleAskedOn is the relation from an ask's node to the block node its row names: the
	// block of the document asks.block_artifact_id holds, addressed as the block index addresses
	// it (AskPlaces.On), `<ref_key of block_artifact_id>#<block_id>`. It is the ask's block pointer
	// as a tuple, beside the part_of to the document that holds the block: asks.block_id is its
	// projection and the document alone is part_of's. The kind is internal to the store:
	// refs.TupleKinds does not list it, and graph_edges does not render it.
	TupleAskedOn = "asked_on"
)

// AskPlaces are the addresses (artifacts.ref_key) of the documents an ask's node relates to, each
// empty where the ask has none: Anchor is the document holding its block, else the document its
// anchor quotes, which graph_edges' old arm reads as coalesce(block_artifact_id, anchor's
// artifact_id); Owner is the document an ask on no issue belongs to (artifact_id); Document is the
// document holding its block (block_artifact_id). Block is the address of the block node the ask
// stands on, empty where the ask has no block or until On sets it.
type AskPlaces struct {
	Anchor, Owner, Document, Block string
}

// On is p with Block the address of the block blockID of the document p.Document: the block
// node as the block index addresses it, the document's ref_key, '#' and the block id
// (text.TargetID). A block of an issue room is held by the room's tree artifact, which is the
// document asks.block_artifact_id names there, so Document is already the room's ref_key. It is
// p with no Block where the ask has no document or blockID is empty.
func (p AskPlaces) On(blockID string) AskPlaces {
	p.Block = ""
	if p.Document != "" && blockID != "" {
		p.Block = p.Document + "#" + blockID
	}
	return p
}

// Fit is whether the three document addresses are inside the store's bound (AddressFits).
// An ask with a document address past the bound has a node, but its row names no node and it
// states none of these relations. That preserves the answers of graph_edges' old arms.
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
// attributes: its node (AskNode), its relations to documents where it has them (AskPlaces), its
// asked_on to the block its row names where the places hold one (AskPlaces.On), and who asked it
// (AuthorFacts: the authored_by tuple, the author's node, and the aliases the server proved)
// where the actor names a person or a session. Every node comes before any tuple a writer
// appends, the ask's node first. marked is whether the asks row names the node: false for
// an ask any of whose relation ends is past the store's bound (AskPlaces.Fit), whose relations are
// left out and whose row the old arms of graph_edges answer. A block whose address alone is past
// the bound is no node, so the ask states no asked_on to it and stays marked, as a comment states
// no anchored_in to such a block.
//
// It is the one statement of an ask's creation, which the three create sites write
// (api's createAskFor and openApprovalAsk, docs' createAskBlock) and the boot step's backfill
// states for a row written before. at is the asks row's created_at, so each relation is dated as
// graph_edges' old arm dated it, and event the event that recorded the ask, or NoEvent for a write
// held for its cause (PlanUncaused).
//
// A later move of the ask - which the restore and the issue room's conversion make - states the
// relations as Claims through WriteArms instead (AskClaims), so the far ends it leaves are
// retracted.
func AskFacts(id, project string, parties []string, author model.Actor, at time.Time, event int64, places AskPlaces) (nodes []Node, tuples []Tuple, marked bool) {
	node := AskNode(id, project, parties, author, at)
	node.SourceEvent = event
	nodes = []Node{node}
	if marked = places.Fit(); marked {
		for _, relation := range askRelations(places) {
			if relation.address == "" || !AddressFits(relation.address) {
				continue
			}
			tuples = append(tuples, Tuple{
				FromKind: KindAsk, FromID: id, Kind: relation.kind,
				ToKind: relation.toKind, ToID: relation.address,
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

// AskClaims is the relations of an ask's node as Claims, for the write that moves an ask
// from where its row stood (the restore, the conversion of an issue into its room): each claim
// retracts the far end the node held under that kind and asserts the new one, and a relation the
// ask no longer has, or whose block address is past the store's bound, retracts what it held. The
// asked_on claim ends the ask's block where the places hold none (AskPlaces.On), so a caller that
// moves an ask whose row names a block sets it.
func AskClaims(id string, places AskPlaces, who model.Actor, at time.Time, event int64) []Claim {
	relations := askRelations(places)
	claims := make([]Claim, 0, len(relations))
	for _, relation := range relations {
		address := relation.address
		if relation.kind == TupleAskedOn && !AddressFits(address) {
			address = ""
		}
		claims = append(claims, Claim{
			FromKind: KindAsk, FromID: id, Kind: relation.kind,
			ToKind: relation.toKind, ToID: address,
			At: at, Who: who, SourceEvent: event,
		})
	}
	return claims
}

// askRelation is one relation of an ask's node to an artifact or a block, by the kind of node its
// far end is, and the far end's address.
type askRelation struct{ kind, toKind, address string }

// askRelations is the relations of places in the order every writer states them: the three to
// documents, then the ask's block.
func askRelations(places AskPlaces) [4]askRelation {
	return [4]askRelation{
		{TupleAnchoredTo, KindArtifact, places.Anchor}, {TupleOwnedBy, KindArtifact, places.Owner},
		{TuplePartOf, KindArtifact, places.Document}, {TupleAskedOn, blockKind, places.Block},
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
