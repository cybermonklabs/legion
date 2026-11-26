package docs

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// planRestores turns the finder's work into the blocks the restore writes, each item read again
// under the issue's lock, since the finder read the whole pass's work before it took any: each
// ask still as the finder read it once its row is locked - its block where the finder saw it, and,
// for a returning ask, answered - keeps its block id unless a block of the spec holds that id as
// the spec's block index holds it now, when it takes a fresh one no document of the issue holds
// (R6's collision rule). An ask block of the decisions document moves whatever its ask's state
// (D1), and every one the document's block index holds moves, one no work item names among them
// (planDocumentMoves). A work item whose row moved since the finder read it is left: its block, if
// the document still holds it, moves as planDocumentMoves reads it under the lock. So is a
// returning ask whose block the spec holds again, which a person put back: a restore of it would
// write a second block of that ask and leave the person's with no row. An answered ask a person
// removed is never written into the spec, in either shape. One whose node holds an asserted
// removed_by since the finder read it is passed over: a returning ask a person took out of the
// spec, and an ask a person deleted from the decisions document while the step ran, which is
// declined with the removal its node holds (nodeRemoval), its row left naming the block it left.
// So is an ask whose block a person's browser deleted from the decisions document's room before a
// settlement versioned the deletion (roomRemovals): its row keeps naming the block it left, and
// writeDecisionsDocument declines it once the version it writes states the removal. A returning
// ask the version that left its block out shows a person removed is declined too (removalOf).
// writeFacts records each other decline on its node, and the result reports every decline. An
// ask the restore moves out of the decisions document moves from the address the finder read
// (RestoreWork.Address), which its moved_to and its node's relations name whether or not the
// document still holds the block (writeFacts). The read of who wrote the version that left a
// block out (removalOf) is the main shape's alone: a block the decisions document holds moves
// whoever put it there, since that document retires and the block has nowhere else to stand.
//
// kept for one release: decisions-step
func (r *issueRestore) planRestores(ctx context.Context, work []graphcensus.RestoreWork) error {
	for _, item := range work {
		row, held := r.rows[item.Ask]
		if !held || row.BlockID == nil || *row.BlockID != item.BlockID || row.BlockArtifactID == nil ||
			item.Shape == graphcensus.RestoreReturns && row.State != asks.StateAnswered {
			slog.Warn("dispatch: the decisions step leaves an ask whose row moved since it was read to its next pass",
				"issue", r.issue, "ask", item.Ask)
			continue
		}
		restore := &restoredBlock{work: item, row: row, id: item.BlockID}
		switch item.Shape {
		case graphcensus.RestoreReturns:
			if *row.BlockArtifactID != r.spec.id {
				continue
			}
			if r.askBlocks[r.spec.id][item.BlockID] {
				slog.Warn("dispatch: the decisions step leaves an ask whose block its spec holds again since it was read",
					"issue", r.issue, "ask", item.Ask)
				continue
			}
			if r.removed[item.Ask] {
				continue
			}
			declined, read, held, err := r.removalOf(ctx, item.Ask, item.BlockID)
			if err != nil {
				return err
			}
			if declined != nil {
				r.decline(declined)
				continue
			}
			restore.read, restore.held = read, held
		case graphcensus.RestoreMoves:
			if r.decisions == nil || *row.BlockArtifactID != r.decisions.id {
				continue
			}
			if r.removed[item.Ask] {
				if _, held := r.roomRemovals[item.Ask]; held {
					continue
				}
				declined, err := r.nodeRemoval(ctx, item.Ask)
				if err != nil {
					return err
				}
				r.decline(declined)
				continue
			}
			restore.from = item.Address
		}
		if r.specBlocks[item.BlockID] || r.specRowHolds(item.BlockID, row.ID) {
			id, err := r.freshBlockID(ctx)
			if err != nil {
				return err
			}
			restore.id = id
		}
		r.restores = append(r.restores, restore)
	}
	if r.decisions != nil {
		if err := r.planDocumentMoves(ctx); err != nil {
			return err
		}
	}
	// Answer-time order, as the restored blocks stand in the spec's `## Decisions` part.
	sort.SliceStable(r.restores, func(a, b int) bool {
		return answeredAt(r.restores[a].row).Before(answeredAt(r.restores[b].row))
	})
	return nil
}

// planDocumentMoves adds a move of the second shape for each ask block the decisions document's
// block index holds (document_blocks of type ask, read under the issue's lock: readState) that no
// restore of this pass takes already (movesBlock): R9's first further kind, a section whose body
// still holds an ask block, has every such block moved first. A block whose row moved since the
// finder read it moves as the block of the row that names it now, from its address in the
// document; a block no row names moves as a block with no row to rebind (rowless), written as the
// document's live tree holds it, from its address there, where the tree still holds it
// (planSections). Each keeps its block id unless the spec holds it, when it takes a fresh one (R6's
// collision rule). A block no row names whose id the spec holds as an ask block - a copy of the
// spec's ask, which has no ask of its own - joins that block instead: no block is written, since
// under a fresh id the spec's next settlement would open it as a new ask beside the one it copies,
// and its address takes its one moved_to to the spec's block, which answers the live ask. A block
// whose row names an ask a person removed (removed) does not move: its ask is never written into
// the spec (planRestores), and a deletion of its block in the room that no settlement has
// versioned is among those (holdDecisionsRoomRemovals).
//
// kept for one release: decisions-step
func (r *issueRestore) planDocumentMoves(ctx context.Context) error {
	for _, id := range slices.Sorted(maps.Keys(r.askBlocks[r.decisions.id])) {
		if r.movesBlock(id) {
			continue
		}
		row, named := r.decisionsRow(id)
		if named && r.removed[row.ID] {
			continue
		}
		restore := &restoredBlock{id: id, work: graphcensus.RestoreWork{
			Shape: graphcensus.RestoreMoves, Issue: r.issue, BlockID: id, Spec: r.spec.refKey,
		}}
		if named {
			restore.row, restore.work.Ask = row, row.ID
			restore.from = refs.BlockAddress(r.decisions.refKey, id)
			restore.work.Address = restore.from
		} else {
			restore.rowless = true
		}
		switch {
		case restore.rowless && r.askBlocks[r.spec.id][id]:
			restore.joins = true
		case r.specBlocks[id] || r.specRowHolds(id, restore.row.ID):
			fresh, err := r.freshBlockID(ctx)
			if err != nil {
				return err
			}
			restore.id = fresh
		}
		r.restores = append(r.restores, restore)
	}
	return nil
}

// movesBlock reports whether a restore of this pass takes the decisions document's block id
// already: a move of the second shape, or a returning ask whose copy the document holds under
// that id, which no row of the document names (planSections).
//
// kept for one release: decisions-step
func (r *issueRestore) movesBlock(id string) bool {
	for _, restore := range r.restores {
		if restore.work.BlockID == id && (restore.work.Shape == graphcensus.RestoreMoves || !r.ownedByDecisions(id)) {
			return true
		}
	}
	return false
}

// decisionsRow is the row of the ask of the issue that names block id of the decisions document.
//
// kept for one release: decisions-step
func (r *issueRestore) decisionsRow(id string) (model.Ask, bool) {
	for _, row := range r.rows {
		if row.BlockArtifactID != nil && *row.BlockArtifactID == r.decisions.id && row.BlockID != nil && *row.BlockID == id {
			return row, true
		}
	}
	return model.Ask{}, false
}

// removalOf reads who took ask's block, blockID, out of the spec (readSpecRemoval). Where that is a
// person it answers the decline, with the version that left the block out, its time and the event
// that recorded it (versionEventID), which the decline records on the ask's node. Otherwise it
// answers what it read and the block as the newest version holding it held it, and the ask is
// restored.
//
// kept for one release: decisions-step
func (r *issueRestore) removalOf(ctx context.Context, ask, blockID string) (*declinedAsk, string, *pmdoc.Node, error) {
	removal, err := readSpecRemoval(ctx, r.tx, r.issue, r.spec.id, r.rows[ask], blockID)
	if err != nil {
		return nil, "", nil, err
	}
	if removal.person == "" {
		return nil, removal.read, removal.held, nil
	}
	event, err := versionEventID(ctx, r.tx, artifactOwner{IssueKey: &r.issue}, r.spec.id, removal.version)
	if err != nil {
		return nil, "", nil, err
	}
	return &declinedAsk{ask: ask, person: removal.person, version: removal.version, at: removal.at, event: event}, "", nil, nil
}

// decline records declined, an answered ask the pass does not write into the spec because a
// person removed its block: writeFacts records the removal on its node, the conversion reads the
// ask as removed (planSection), and the result, which the step logs, reports it.
//
// kept for one release: decisions-step
func (r *issueRestore) decline(declined *declinedAsk) {
	r.declined = append(r.declined, declined)
	r.removed[declined.ask] = true
	r.result.Declined = append(r.result.Declined, DeclinedAsk{Ask: declined.ask, Person: declined.person, Version: declined.version})
}

// nodeRemoval is the decline of ask, an ask of the second shape whose node holds an asserted
// removed_by (readState's removed): a person deleted its block from the decisions document since
// the finder read it. It is that removal as the node's newest removed_by states it - the person,
// the time and the event, the artifact.version of the person's version that left the block out -
// with that version's number, 0 where the event names none. writeFacts states the removal again
// under that event, which appends nothing: the store's repeat key (graph_tuples_once) holds it
// already, so the removal stands as the person's write stated it.
//
// kept for one release: decisions-step
func (r *issueRestore) nodeRemoval(ctx context.Context, ask string) (*declinedAsk, error) {
	declined := &declinedAsk{ask: ask}
	if err := r.tx.QueryRow(ctx, `
		select newest.to_id, newest.at, newest.source_event, coalesce((e.payload->'version'->>'number')::int, 0)
		from (
			select t.to_id, t.at, t.source_event from graph_tuples t
			where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind <> $4
			order by t.seq desc limit 1
		) newest
		left join events e on e.id = newest.source_event and e.type = 'artifact.version'
	`, graphstore.KindAsk, ask, graphstore.TupleRemovedBy, graphstore.KindValue).Scan(
		&declined.person, &declined.at, &declined.event, &declined.version); err != nil {
		return nil, fmt.Errorf("read the removal the node of ask %s holds: %w", ask, err)
	}
	return declined, nil
}

// specRemoval is who took an ask's block out of its spec, as readSpecRemoval reads it: the person
// whose version left it out, that version and its time, person "" where no person did; and
// otherwise what the read found (read, one of the restoreRead reasons) and the block as the newest
// version holding it held it (held), nil where none did.
type specRemoval struct {
	person  string
	version int
	at      time.Time
	read    string
	held    *pmdoc.Node
}

// readSpecRemoval reads, through q, who took ask's block, blockID, out of the spec specID of
// issue: the version that left it out, the lowest above the newest whose stored tree holds an ask
// block with that id, read newest first through the stored-version reader the approval carry
// reads (approvedVersion), a version it cannot read holding none. Where that version is a
// person's (versionPerson) it names that person, the version and its time. Otherwise it names what
// it read, and the ask is restored: that version's authors name no person and no agent acting
// with the person scope, no version of the spec held the block, or that version names no author;
// with the block as the newest version holding it held it, nil where none did, whose bytes the
// restore writes back (restoredAskBlock). A version a person's own write wrote is the person's
// whatever other authors it lists, and one a session wrote with no act of a person - every
// agent's drop the restore exists to undo, the release before's bearer writes among them - is no
// person's.
//
// The ask's answerer is no remover in the one version that first holds her answer: the version
// written after the ask's answered_at whose previous version was written before it. The release
// before wrote an answer into its block in the live document in the answerer's name, so the first
// version after the answer credits her whatever else it holds, an agent's drop of the block among
// it (answerersVersion). In every later version she counts as any person does. This build's own
// settlement records a person's removal of an answered ask's block as the version is written
// (holdRoomRemovals), and so does every write that versions the room in a settlement's place - an
// upload, a snapshot, a named version - so a version this build wrote with the block gone, its
// answerer among the room's authors, has its ask declined before this reads it (readState's
// removed).
//
// It is the one test of a person's removal, which the restore reads (removalOf) and the store
// census's dangling counter reads its returning set by (ReturnDeclined). It writes nothing.
func readSpecRemoval(ctx context.Context, q Queryer, issue, specID string, ask model.Ask, blockID string) (specRemoval, error) {
	rows, err := q.Query(ctx, `select number from artifact_versions where artifact_id = $1::uuid order by number desc`, specID)
	if err != nil {
		return specRemoval{}, fmt.Errorf("read the versions of the spec of %s: %w", issue, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return specRemoval{}, fmt.Errorf("read the versions of the spec of %s: %w", issue, err)
	}
	removing, holder := 0, 0
	var held *pmdoc.Node
	for _, number := range numbers {
		tree, _, readable, err := approvedVersion(ctx, q, specID, number)
		if err != nil {
			return specRemoval{}, err
		}
		if readable {
			if held = askBlockIn(tree, blockID); held != nil {
				holder = number
				break
			}
		}
		removing = number
	}
	switch {
	case holder == 0:
		return specRemoval{read: restoreReadNeverHeld}, nil
	case removing == 0:
		// The newest version holds the block: no version left it out.
		return specRemoval{read: restoreReadNoPerson, held: held}, nil
	}
	var authors []model.Actor
	var at, previousAt time.Time
	if err := q.QueryRow(ctx, `
		select v.authors, v.created_at, previous.created_at
		from artifact_versions v, artifact_versions previous
		where v.artifact_id = $1::uuid and v.number = $2 and previous.artifact_id = $1::uuid and previous.number = $3
	`, specID, removing, holder).Scan(&authors, &at, &previousAt); err != nil {
		return specRemoval{}, fmt.Errorf("read the authors of version %d of the spec of %s: %w", removing, issue, err)
	}
	if len(authors) == 0 {
		return specRemoval{read: restoreReadNoAuthors, held: held}, nil
	}
	person, _ := versionPerson(authors, answerersVersion(ask, at, previousAt))
	if person == "" {
		return specRemoval{read: restoreReadNoPerson, held: held}, nil
	}
	return specRemoval{person: person, version: removing, at: at}, nil
}

// ReturnDeclined answers whether the restore declines to write back the ask of work, a return of
// the main shape (graphcensus.RestoreReturns): whether a person took its block out of the spec, as
// the restore reads it before it writes one back (readSpecRemoval, removalOf). It reads the ask's
// row and the spec work names through q and writes nothing, so the store census runs it in its
// read-only snapshot (graphcensus.ReturnDeclined), and the dangling counter's returning set holds
// only the asks the restore writes back.
func ReturnDeclined(ctx context.Context, q graphstore.Queryer, work graphcensus.RestoreWork) (bool, error) {
	var specID string
	ask, err := ScanAsk(q.QueryRow(ctx, `
		select `+AskColumns+`, spec.id::text from asks a
		join artifacts spec on spec.ref_key = $2
		where a.id = $1::uuid`, work.Ask, work.Spec), &specID)
	if err != nil {
		return false, fmt.Errorf("read the ask %s the restore returns to %s: %w", work.Ask, work.Spec, err)
	}
	removal, err := readSpecRemoval(ctx, q, work.Issue, specID, ask, work.BlockID)
	if err != nil {
		return false, err
	}
	return removal.person != "", nil
}

// answerersVersion is the login of ask's answerer where a version written at at, whose previous
// version was written at previousAt, is the one that first holds the answer - written after the
// answer's time, its previous version before it - and "" otherwise (removalOf). Its one caller,
// readSpecRemoval, serves the store census's dangling counter as well as the restore
// (ReturnDeclined).
func answerersVersion(ask model.Ask, at, previousAt time.Time) string {
	if ask.Answer == nil || !at.After(ask.Answer.At) || !previousAt.Before(ask.Answer.At) {
		return ""
	}
	return model.CanonicalLogin(ask.Answer.User)
}

// versionPerson is the person a version's authors name, "" where they name none, and the author
// that names them: the first author that is a person (kind user, a browser's person, or an agent
// acting for one through a route that takes the person scope) or an agent whose recorded act
// holds the person scope (a broker-issued token its person scoped), named as that author's person
// (personOf), other than excluded, the answerer in the version that first holds her answer
// (answerersVersion).
func versionPerson(authors []model.Actor, excluded string) (string, model.Actor) {
	for _, author := range authors {
		if author.Kind != "user" && (author.Act == nil || !slices.Contains(author.Act.Scopes, model.ScopePerson)) {
			continue
		}
		if login := personOf(author); login != "" && login != excluded && graphstore.AddressFits(login) {
			return login, author
		}
	}
	return "", model.Actor{}
}

// askBlockIn is the ask block tree holds under id, nil where it holds none.
func askBlockIn(tree *pmdoc.Node, id string) *pmdoc.Node {
	var held *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if blockID, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "ask" && blockID == id {
			held = node
		}
		return held == nil
	})
	return held
}

// specRowHolds reports whether the row of an ask of the issue other than ask names the spec's
// block id, which asks_block_id_unique keeps to one row: an ask a person declined or a settlement
// retracted keeps its row naming the block it left, so the restored ask takes a fresh id there as
// it does where a live block of the spec holds the id (R6).
//
// kept for one release: decisions-step
func (r *issueRestore) specRowHolds(id, ask string) bool {
	for _, row := range r.rows {
		if row.ID != ask && row.BlockArtifactID != nil && *row.BlockArtifactID == r.spec.id && row.BlockID != nil && *row.BlockID == id {
			return true
		}
	}
	return false
}

// answeredAt is when ask was answered, the zero time for an ask its row holds no answer of.
//
// kept for one release: decisions-step
func answeredAt(ask model.Ask) time.Time {
	if ask.Answer == nil {
		return time.Time{}
	}
	return ask.Answer.At
}

// freshBlockID is a block id no document of the issue holds, as a block or as an ask's row.
//
// kept for one release: decisions-step
func (r *issueRestore) freshBlockID(ctx context.Context) (string, error) {
	for {
		id := uuid.NewString()
		var held bool
		if err := r.tx.QueryRow(ctx, `
			select exists (
				select 1 from document_blocks b join artifacts a on a.id = b.artifact_id
				where a.issue_key = $1 and b.block_id = $2
			) or exists (select 1 from asks k where k.issue_key = $1 and k.block_id = $2)
		`, r.issue, id).Scan(&held); err != nil {
			return "", fmt.Errorf("read whether a document of %s holds a block id: %w", r.issue, err)
		}
		if !held {
			return id, nil
		}
	}
}

// restoreOf is the restore of ask, nil where this pass restores none.
//
// kept for one release: decisions-step
func (r *issueRestore) restoreOf(ask string) *restoredBlock {
	for _, restore := range r.restores {
		if !restore.rowless && restore.row.ID == ask {
			return restore
		}
	}
	return nil
}

// askBlockOf is the document that holds ask's block and the block's id, and whether it has one:
// the spec, for an ask this pass restores; else the document of the issue its row names, where
// that document's block index holds the block as an ask block (R9: the ask's "restored or present"
// block). The decisions document is none of them: an ask its row names there is the restore's,
// which moves it into the spec whatever its state.
//
// kept for one release: decisions-step
func (r *issueRestore) askBlockOf(ask string) (issueDocument, string, bool) {
	if restore := r.restoreOf(ask); restore != nil {
		return r.spec, restore.id, true
	}
	row, held := r.rows[ask]
	if !held || row.State != asks.StateAnswered || row.BlockID == nil || row.BlockArtifactID == nil ||
		r.decisions != nil && *row.BlockArtifactID == r.decisions.id || !r.askBlocks[*row.BlockArtifactID][*row.BlockID] {
		return issueDocument{}, "", false
	}
	document, stands := r.documents[*row.BlockArtifactID]
	if !stands {
		return issueDocument{}, "", false
	}
	return document, *row.BlockID, true
}

// ownedByDecisions reports whether an ask names a block of the decisions document under id as its
// own: an ask this pass moves out of it, or one whose row names that document.
//
// kept for one release: decisions-step
func (r *issueRestore) ownedByDecisions(id string) bool {
	for _, restore := range r.restores {
		if restore.work.Shape == graphcensus.RestoreMoves && restore.work.BlockID == id {
			return true
		}
	}
	_, named := r.decisionsRow(id)
	return named
}
