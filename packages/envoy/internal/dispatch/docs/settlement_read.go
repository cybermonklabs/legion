package docs

import (
	"context"
	"fmt"
)

// documentRemovedStatement reads whether a document's artifacts row has gone.
const documentRemovedStatement = `select not exists(select 1 from artifacts where id = $1)`

// documentRemoved reports whether the document this settlement stopped short on has been
// removed since it started (settleRoomWithin). The read runs through the settlement's own
// transaction once it has one, since a pooled read beside it is the second connection that
// deadlocks the pool, and through the pool before. In that transaction it is a statement of its
// own, so it reads a removal that committed while an earlier statement waited behind it. A read
// that fails, as every statement of a transaction an error aborted does, answers false, and the
// settlement stops short as it would have.
func (st *settlement) documentRemoved(ctx context.Context) bool {
	var q Queryer = st.service.store.Pool
	if st.tx != nil {
		q = st.tx
	}
	var removed bool
	if err := q.QueryRow(ctx, documentRemovedStatement, st.room).Scan(&removed); err != nil {
		return false
	}
	return removed
}

// readLocked is what the settlement reads once its transaction holds the document's owner lock: the
// latest version, the document's cursor lock, the catch-up that brings the room's instance to the
// document's durable head, the tree it reconciles and the rendering of that tree, and whom its
// version credits.
//
// It is given both of the settlement's contexts: ctx carries this settlement's ledger, cursor and
// transaction, and is what every read here runs under; parent is the lifetime the reload of a room
// whose stored history was replaced is given, once this transaction has been rolled back.
func (st *settlement) readLocked(ctx context.Context, parent context.Context) (settleStep, settleOutcome) {
	s, room, state, tx := st.service, st.room, st.state, st.tx
	latest, err := latestVersion(ctx, tx, room)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	st.latest = latest
	if err := s.lockSettlementCursor(ctx, tx, room); err != nil {
		if s.shuttingDown(room) {
			s.log().Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return settleDone, settleOutcome{}
		}
		return settleDone, st.stoppedShort(fmt.Errorf("lock document cursor: %w", err))
	}
	if s.afterSettleLock != nil {
		s.afterSettleLock(room)
	}
	// The first read of a refused update's second order (roomRefusedUpdate): the room's
	// persistence worker met the append gate's refusal before this settlement took the
	// document's advisory lock, so the room's tree holds an update no store write of this
	// settlement will meet. Read once the lock is held, since from here to the commit no append
	// of the room is refused.
	if st.roomRefusedUpdate() {
		return settleDone, st.endOnRefusedUpdate()
	}
	head, err := currentUpdateCursor(ctx, tx, room)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	// The gate, before this settlement reads the document or writes anything. The room it holds
	// must stand at that head: with two envoy-dispatch processes on one Postgres, an update the
	// other process appended can be stored before this room's catch-up has taken it in, and
	// settling from it corrupts three things at once. Its version would record a
	// doc_update_version at or past that head over text that is missing the other process's
	// update, so contentChangedSinceVersion then reports nothing past that cursor and those words
	// are never versioned; clearSettlementPending would delete the one durable record that the
	// work is still owed; and reconcileAskBlocks would retract every open ask whose block this
	// stale tree cannot see.
	//
	// So a room behind the head is caught up here, under the document's advisory lock this
	// transaction holds, reading through that transaction (catchUpRoom): no append can land
	// meanwhile, so the catch-up always brings the room to that head, and the settlement
	// proceeds. The rows it applies are another process's content change, which re-arms the
	// room's settlement when one is armed or running (rearmSettle) - this one is - and that moves
	// the room's generation, so this one writes no version (the supersession check in applyStamp)
	// and the one it re-armed versions every update, this process's and the other's. Shutdown's
	// settlement re-arms nothing and versions the caught-up room itself, since nothing is left for
	// the next process. A history replaced under the room is the one case catch-up cannot mend:
	// the transaction rolls back, which keeps doc_settlements_pending and releases the lock the
	// room's close needs, and the room is closed and reloaded (reloadReplacedRoom).
	//
	// The cursor is the one this settlement marked its context with, which its repairs' appends
	// are gated on (appendingCursor), so the two can never disagree.
	if st.cursor != nil && st.cursor.heldThrough() != head {
		outcome, err := s.catchUpRoom(ctx, tx, room, st.doc)
		switch {
		case err != nil:
			return settleDone, st.stoppedShort(err)
		case outcome == catchUpGone:
			// The room no longer holds the instance this settlement took, or a close holds it. As
			// in take, the retry this settlement stops short into decides by the room's generation:
			// after a close that moved it, the document is left to what that close arranged; after
			// one that did not, the retry arms the settlement again, and that settlement, once this
			// one's rollback has let the close finish, loads the room at the head.
			return settleDone, st.stoppedShort(fmt.Errorf("catch the room up to the durable head: %w", errRoomReplaced))
		case outcome == catchUpReplaced:
			// The rollback releases the document's advisory lock, which the room's close needs: a
			// close flushes the room's persistence, whose appends take that lock.
			_ = tx.Rollback(ctx)
			refusal := fmt.Errorf("%w: room %s holds the document through update %d, and the stored history was replaced under it (head %d)",
				ErrRoomBehindHead, room, st.cursor.heldThrough(), head)
			if parent.Err() != nil {
				return settleDone, st.stoppedShort(refusal)
			}
			s.reloadReplacedRoom(parent, room, state, st.generation, st.doc, refusal)
			return settleDone, settleOutcome{next: settleReloaded}
		}
		if through := st.cursor.heldThrough(); through != head {
			return settleDone, st.stoppedShort(fmt.Errorf("%w: room %s holds the document through update %d after its catch-up, the stored document stands at %d",
				ErrRoomBehindHead, room, through, head))
		}
	}
	// The settlement reads the document three times, and each read has its own name: read is this
	// one, before the database work; reconciled is the tree the ask blocks are reconciled on, the
	// stamp's when it stamped ids; versioned is the one the version is rendered from, read again
	// after the repairs when the settlement wrote any. A browser's edit made in between is in a
	// later one and not an earlier one. read and versioned are each the room at an exact commit
	// position (readSettlementTree), every update through which this transaction stores before it
	// versions, and the stamp reads inside its own transaction: the room's peers and the service
	// can write it while a walk of the live tree, which takes no lock, reads it, and a torn read
	// would be versioned as the document. Each bounds the in-flight credits its authors take before
	// it reads, so a normal credit, which follows its room update, is for a change the read holds.
	// The one change a read can lack whose author it takes is a committed transaction's write,
	// credited at its commit and published after the read; the supersession check in applyStamp
	// then writes no version, since the write still holds its slot or its publish moved the
	// generation.
	first, err := s.readSettlementTree(state, st.doc, room, s.afterSettleAuthorsTake, false)
	if err != nil {
		if s.skipUnreadableSettlement(room, err) {
			return settleDone, settleOutcome{}
		}
		return settleDone, st.stoppedShort(err)
	}
	st.first, st.read = first, first.tree
	if s.afterSettleTreeRead != nil {
		s.afterSettleTreeRead(room, st.read)
	}
	// Whom the ask blocks' reconciliation names, as the document's pending authors stand before
	// this settlement stores anything. It stores nothing until it has reconciled them: the copy
	// lock that reconciliation can take (copiedAskSources, owedCopiesOf) comes before any
	// pending-settlement row, which each append writes (updateOwedStatement), in every transaction
	// (lockProjectCopies). Its version's authors are taken again once it has stored the room's
	// updates it holds (settlementCredit).
	credit, err := settlementCredit(ctx, tx, state, room, first)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	st.credit = credit

	// What the document renders as read. The version reuses this rendering, offsets included,
	// when it is rendered from this same tree unchanged (commitVersion).
	st.markdown, st.offsets, st.renderErr = renderTreeWithOffsets(st.read)
	if s.afterSettleRead != nil {
		s.afterSettleRead(room)
	}
	return settleOn, settleOutcome{}
}
