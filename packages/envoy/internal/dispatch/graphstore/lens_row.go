package graphstore

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// AttributeArtifactID is the row a lens on a document, or on a block of one, records: the
// artifacts.id of the row the artifacts table holds at the document's address as the write that
// places the lens there runs - not the row the document's node was written for - a value of the
// lens (migration 20261123174710 declares it on lens, beside verdict), since a row's id is the
// address of no node. A document's address, `<issue key>/<slug>`, is a name a later document
// takes again, and a row's id is never reused, so a lens whose recorded row no longer stands at
// its document's address stands on a document that is no more (refs.StaleSQL). The store's write
// of a lens's part_of states it (queueLensArtifactIDs), whichever writer places the lens, the
// invoke and a move alike, and retracts it where a write moves the lens off a document.
const AttributeArtifactID = "artifact_id"

// placesLens is whether a relation of kind out of a node of fromKind places a lens: its part_of,
// whose write records the row of the document it places the lens on (queueLensArtifactIDs).
func placesLens(kind, fromKind string) bool {
	return kind == TuplePartOf && fromKind == KindLens
}

// readInWritePhase is whether a write reads its relation of kind, out of a node of fromKind at the
// node toKind and toID name, in its own write phase: a relation the pointer type rules hold
// (pointerRuleOf), a part_of at a kind that takes a node reserved for its author
// (placesUnderAuthorOnly), and a lens's part_of (placesLens). The arms of a caller's statement
// refuse each such relation (refusePointersInArms), and a write held for its cause copies the
// held tuples it reads for it (heldTuplesRead). An attribute, whose far end is a value, is none.
func readInWritePhase(kind, fromKind, toKind, toID string) bool {
	return toKind != KindValue && (pointerRuleOf(kind, fromKind) != nil ||
		placesUnderAuthorOnly(kind, toKind, toID) || placesLens(kind, fromKind))
}

// ErrNoDocumentRow is a write that places a lens on a document, or on a block of one, at whose
// address the artifacts table holds no row as the write runs: the document is gone, a removal took
// its row while the write queued behind it, so the write would record no row and the lens would
// stand on nothing. It is a sentinel so a route can answer it as a write on a gone node, 409
// NODE_GONE; what a write is refused with is a NoDocumentRowError, which wraps it and names the
// lens and the node.
var ErrNoDocumentRow = errors.New("no document row stands where the write places a lens")

// NoDocumentRowError is ErrNoDocumentRow for one lens: its id and the node, as the part_of names it
// by kind and id, the write places it on.
type NoDocumentRowError struct{ Lens, ToKind, ToID string }

func (e *NoDocumentRowError) Error() string {
	return fmt.Sprintf("the lens %s is placed on the %s %s, at whose address no document row stands", e.Lens, AddressKind(e.ToKind, e.ToID), e.ToID)
}

func (e *NoDocumentRowError) Unwrap() error { return ErrNoDocumentRow }

// queueLensArtifactIDs queues on b, between the node write and the append, beside the reads of the
// pointer check (pointerCheck.queue), the one statement that records on each lens a part_of of
// tuples places the artifacts.id of the row of the document it places the lens on
// (AttributeArtifactID): every placement of a lens records it, the write that invokes a lens and a
// move under a new parent alike, whichever writer appends the part_of, so no lens stands on a
// document with the row of another (lensArtifactIDsStatement). A lens two part_of tuples of one
// write place is placed by the last. A placement on a document no row stands at is refused, the
// statement's result (NoDocumentRowError). A write that places no lens queues nothing. Its
// arguments build at Send, as the append's do, so a placement that cannot be encoded fails Send.
func queueLensArtifactIDs(b *store.WriteBatch, tuples []Tuple) {
	last := map[string]int{}
	for index := range tuples {
		if tuple := &tuples[index]; tuple.Asserted && tuple.ToKind != KindValue && placesLens(tuple.Kind, tuple.FromKind) {
			last[tuple.FromID] = index
		}
	}
	if len(last) == 0 {
		return
	}
	placements := make([]Tuple, 0, len(last))
	for _, lens := range slices.Sorted(maps.Keys(last)) {
		placements = append(placements, tuples[last[lens]])
	}
	b.QueueBuilt("record the artifact_id of each lens the write places", lensArtifactIDsStatement, func() ([]any, error) {
		return tupleParams(placements)
	}).Query(func(rows pgx.Rows) error {
		if !rows.Next() {
			return nil
		}
		refused := &NoDocumentRowError{}
		if err := rows.Scan(&refused.Lens, &refused.ToKind, &refused.ToID); err != nil {
			return err
		}
		return refused
	})
}

// lensArtifactIDsStatement is queueLensArtifactIDs' statement, over the placements it binds as a
// tuple append binds its tuples (tupleParams), one for each lens, in ascending id. A lens placed on
// a document, or on a block of one - a block whose id is no finding's (FindingIDSQL) and no block of
// an ask, a comment or a message - records the row the artifacts table holds, as the write runs, at
// the address of that document: the node's id for a document, and for a block the address before
// the `#` of the block's id, which is its own address, `<ref_key>#<block id>`. That row is the one
// whose ref_key, the trigger's `<issue key>/<slug>`, `<project key>/<slug>` or
// `agent/<session>/<slug>`, is that address (one probe of its unique index). It reads neither the
// row a node at the address was written for, which may be a removed one's, nor the node a block
// stands under through its own part_of. Where the lens's row holds another value than that one, the
// statement moves the row's attribute to it and appends its value tuple, at the placement's time,
// writer and cause: the row's id where the lens stands on a document, and where it does not - a lens
// moved off a document onto any other node, a finding of another lens, a block of an ask - the
// retraction of the value it held, so the lens records none. A lens whose value is the one it holds
// already states nothing. A lens placed on a document no row stands at states nothing either, and
// the statement answers it, which refuses the write. It takes no row lock of its own and reads each
// lens's row and the artifacts row plainly. The lens route takes the issue's owner lock first
// (api's lockIssueOwner), as does the pre-mortem conversion (api's lockReasoningOwners). That lock
// serializes placements, so no other placement moves a lens's row between this statement's read
// of it and its update.
var lensArtifactIDsStatement = `
	with placed as (
		select placement.from_id as lens, placement.to_kind, placement.to_id, placement.at, placement.who,
		       ` + causeOf("placement.source_event") + ` as source_event,
		       (placement.to_kind = '` + KindArtifact + `'
		        or placement.to_kind = '` + blockKind + `'
		           and not ` + FindingIDSQL("placement.to_id") + `
		           and split_part(placement.to_id, '/', 1) not in ('` + KindAsk + `', '` + KindComment + `', '` + KindMessage + `')) as on_document
		from unnest(` + tupleArrays(1) + `) as placement (` + tupleWriteColumns + `)
	),
	placements as (
		select placed.lens, placed.to_kind, placed.to_id, placed.at, placed.who, placed.source_event, placed.on_document,
		       held.attributes->>'` + AttributeArtifactID + `' as held_row,
		       case when placed.on_document then
		           (select document.id::text from artifacts document where document.ref_key = split_part(placed.to_id, '#', 1))
		       end as standing_row
		from placed
		join graph_nodes held on held.kind = '` + KindLens + `' and held.id = placed.lens
	),
	changed as (
		select * from placements
		where placements.standing_row is distinct from placements.held_row
		  and not (placements.on_document and placements.standing_row is null)
	),
	moved as (
		update graph_nodes held
		set attributes = case when changed.standing_row is null then held.attributes - '` + AttributeArtifactID + `'
			else held.attributes || jsonb_build_object('` + AttributeArtifactID + `', changed.standing_row) end
		from changed
		where held.kind = '` + KindLens + `' and held.id = changed.lens
	),
	appended as (
		insert into graph_tuples (` + tupleWriteColumns + `)
		select '` + KindLens + `', changed.lens, '` + AttributeArtifactID + `', '` + KindValue + `',
		       coalesce(changed.standing_row, changed.held_row), changed.at, changed.who, changed.source_event,
		       null, changed.standing_row is not null
		from changed
		order by changed.lens
		on conflict do nothing
	)
	select placements.lens, placements.to_kind, placements.to_id from placements
	where placements.on_document and placements.standing_row is null
	order by placements.lens`
