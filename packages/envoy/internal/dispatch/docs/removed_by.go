package docs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// dropper is who makes a write that may take an ask block out of its document: whether the write
// is a person's own (writtenByPerson), and so may take an answered ask's block out, and, for such
// a write, the person whose removal it records on each answered ask it takes out
// (graphstore.TupleRemovedBy), "" where it records none. actor is the write's own actor, which
// the removal's tuple names as its writer.
type dropper struct {
	person  bool
	remover string
	actor   model.Actor
}

// dropperOf is who makes actor's write in ctx: a person's own write (writtenByPerson) records its
// removals as the person it is made for (personOf).
func dropperOf(ctx context.Context, actor model.Actor) dropper {
	if !writtenByPerson(ctx, actor) {
		return dropper{actor: actor}
	}
	return dropper{person: true, remover: personOf(actor), actor: actor}
}

// personOf is the person a person's own write is made for, as an answer's decided_by names the
// person who answered: the browser's person (a user actor's login), or the person a credential
// holding the person scope belongs to (the owner a personal token's or a broker-issued token's
// actor names). It is "" where actor names neither.
func personOf(actor model.Actor) string {
	if actor.Kind == "user" {
		return model.CanonicalLogin(actor.ID)
	}
	if actor.Owner != nil {
		return model.CanonicalLogin(*actor.Owner)
	}
	return ""
}

// heldRemoval is one answered ask a person's own write took the block of out of its document,
// held for the event that records the version the write leaves (removalsOf).
type heldRemoval struct {
	person string
	actor  model.Actor
	at     time.Time
}

// answerDroppedAnsweredAsks answers the answered asks of holder (documentAsksOf, roomAsksOf) whose
// block a write loses (lost; after is the tree the write leaves). A write that is not a person's
// own is refused for them (refuseDroppedAnsweredAsks). A person's own write is taken, and holds each
// as that person's removal on the transaction's ledger, under the document holding the ask's block
// (refs.AskBlockDocumentSQL), which the version the write leaves of that document states on the
// ask's node (removalsOf); a removal the transaction holds already stays as the write that held it
// first held it. standing narrows the removals, and the refusal, to the asks whose block
// the block index holds as an ask block, for a write that replaces the document whole and so has
// no tree of its own to say what the document held before it: an ask whose block an earlier write
// took out - a person's removal, or an agent's drop the restore still owes - is lost to no write of
// this one.
func answerDroppedAnsweredAsks(ctx context.Context, q Queryer, holder askHolder, after *pmdoc.Node, lost func(blockID string) bool, by dropper, standing bool) error {
	if !by.person {
		return refuseDroppedAnsweredAsks(ctx, q, holder, after, lost, standing)
	}
	ledger := ledgerFrom(ctx)
	if by.remover == "" || !graphstore.AddressFits(by.remover) || ledger == nil {
		return nil
	}
	rows, err := q.Query(ctx, `
		select k.id::text, k.block_id, `+refs.AskBlockDocumentSQL("k")+`::text from asks k
		where `+holder.predicate("k", "$1::uuid")+` and k.state = 'answered' and k.block_id is not null
		  and (not $2 or exists (
			select 1 from document_blocks d
			where d.artifact_id = k.block_artifact_id and d.block_id = k.block_id and d.type = 'ask'))
		order by k.block_id
	`, holder.id, standing)
	if err != nil {
		return fmt.Errorf("read the answered asks a person's write removes: %w", err)
	}
	asks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([3]string, error) {
		var ask [3]string
		err := row.Scan(&ask[0], &ask[1], &ask[2])
		return ask, err
	})
	if err != nil {
		return fmt.Errorf("read the answered asks a person's write removes: %w", err)
	}
	at := time.Now().UTC()
	for _, ask := range asks {
		if !lost(ask[1]) {
			continue
		}
		document := ask[2]
		if _, held := ledger.removals[document][ask[0]]; held {
			continue
		}
		if ledger.removals == nil {
			ledger.removals = map[string]map[string]heldRemoval{}
		}
		if ledger.removals[document] == nil {
			ledger.removals[document] = map[string]heldRemoval{}
		}
		ledger.removals[document][ask[0]] = heldRemoval{person: by.remover, actor: by.actor.Recorded(), at: at}
	}
	return nil
}

// holdRoomRemovals holds, for a version that takes room's live text, the removal of each answered
// ask whose block the document's block index holds and live, the room as that version reads it,
// does not hold as an ask block, as the person authors names (versionPerson): the authors of the
// room's edits no version has listed, which a settlement's version lists. No writer but a
// person's own editor takes an answered ask's block out of a document in the room: every other
// write comes through a route that refuses it (refuseDroppedAnsweredAsks) and writes a version of
// its own. So the browser's deletion is that person's removal, as the same deletion over HTTP is
// at once (answerDroppedAnsweredAsks), and it stays taken: the version's index pass states it on
// the ask's node (removalsOf), which the restore then declines (R6).
//
// The settlement that versions the room holds it (settlement.go), and so does each write that
// versions the room in a settlement's place, before the settlement runs: an upload (ReplaceText),
// whose version lists the uploader alone and clears the room's pending authors, and a snapshot or
// a named version (SnapshotVersion, NamedVersion), whose version lists them beside its writer.
// The settlement after either finds the room at that version's text and writes none, and the
// restore reads no person in an upload's authors, and none in the answerer's own first version
// (answerersVersion) - an answer writes the ask's state into its block in her name
// (KeepAskBlockInStep), so her deletion settled into that version is hers as well - and would
// write the block back. live is the room as it stood before the transaction's own operations
// (roomReadTree): what those took out is theirs to hold or refuse (answerDroppedAnsweredAsks), and
// a nil live, a room no read of the transaction's records, holds nothing. Authors that name no
// person hold none, and a removal the transaction holds already stays as its write held it.
func holdRoomRemovals(ctx context.Context, q Queryer, artifactID string, live *pmdoc.Node, authors []model.Actor) error {
	if live == nil {
		return nil
	}
	person, author := versionPerson(authors, "")
	if person == "" {
		return nil
	}
	held := openAskBlockIDs(live)
	return answerDroppedAnsweredAsks(ctx, q, documentAsksOf(artifactID), live, func(blockID string) bool {
		_, kept := held[blockID]
		return !kept
	}, dropper{person: true, remover: person, actor: author}, true)
}

// roomReadTree is the room a version of artifactID takes, as holdRoomRemovals compares it with the
// block index: versioned, the tree the version takes, where the transaction holds no write to the
// document or one that has changed nothing, and otherwise the tree that write read before its
// first operation (liveWrite.readTree), nil where its first operation recorded none (a margin
// write's), which holds nothing.
func roomReadTree(ctx context.Context, artifactID string, versioned *pmdoc.Node) *pmdoc.Node {
	write := joinedLiveWrite(ctx, artifactID)
	if write == nil || len(write.updates) == 0 {
		return versioned
	}
	return write.readTree
}

// removalsOf is what a version of artifactID states of the removals a person's own write of this
// transaction held for that document (answerDroppedAnsweredAsks): the person's node, and one
// asserted removed_by from each ask's node to it, planned for the event that records the version
// (graphstore.PlanUncaused) and written nowhere here. The version's index pass plans it into its
// own stated write (refs.PlanBlocks, refs.PlanDecisionNodes, beside), so that write's node write
// writes the person's node beside the block nodes, under the one mark, and the caller's
// graphstore.QueueCaused appends the removed_by tuples, with what the index stated, under the
// version's event; a version no index pass plans queues it on its own (queueRemovals). The
// removals are taken off the ledger, so a second version of the same transaction states none of
// them again. It is the zero write where the transaction holds no removal of the document.
func removalsOf(ctx context.Context, artifactID string) graphstore.Uncaused {
	ledger := ledgerFrom(ctx)
	if ledger == nil || len(ledger.removals[artifactID]) == 0 {
		return graphstore.Uncaused{}
	}
	held := ledger.removals[artifactID]
	delete(ledger.removals, artifactID)
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	people := map[string]bool{}
	for _, ask := range slices.Sorted(maps.Keys(held)) {
		removal := held[ask]
		if !people[removal.person] {
			people[removal.person] = true
			nodes = append(nodes, graphstore.PersonNode(removal.person, removal.actor, removal.at, graphstore.NoEvent))
		}
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: ask, Kind: graphstore.TupleRemovedBy,
			ToKind: graphstore.KindPerson, ToID: removal.person,
			At: removal.at, Who: removal.actor, SourceEvent: graphstore.NoEvent, Asserted: true,
		})
	}
	return graphstore.PlanUncaused(nodes, tuples)
}

// queueRemovals is removalsOf for a version no index pass plans - one with no rendering
// (indexDocument) - which carries the removals on its own: their node write, the person's node, is
// queued here through write, after the version's insert, and the write planned is answered for
// the caller's event as an index pass's would be, which appends the removed_by tuples under it
// (graphstore.QueueCaused).
func queueRemovals(ctx context.Context, tx pgx.Tx, artifactID string) (graphstore.Uncaused, error) {
	removals := removalsOf(ctx, artifactID)
	if len(removals.Nodes) == 0 && len(removals.Tuples) == 0 {
		return graphstore.Uncaused{}, nil
	}
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, removals)
		return nil
	}); err != nil {
		return graphstore.Uncaused{}, fmt.Errorf("state the removals of the answered asks of %s: %w", artifactID, err)
	}
	return removals, nil
}
