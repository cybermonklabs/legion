package docs

import (
	"context"
	"fmt"
	"log/slog"
)

// documentRemovedStatement reads whether a document's artifacts row has gone.
const documentRemovedStatement = `select not exists(select 1 from artifacts where id = $1)`

// documentRemoved reports whether the document this settlement stopped short on has been
// removed since it started (settleRoomWithin), read through q: the transaction of the pass under
// way once it has one, since a pooled read beside it is the second connection that deadlocks the
// pool, and the settlements pool before (stoppedShort). It never reads through the request pool:
// a settlement that could not open its own pool makes no such read (removedThroughPool). In that
// transaction it is a statement of its own, so it reads a removal that committed while an earlier
// statement waited behind it. A read that fails, as every statement of a transaction an error
// aborted does, answers false, and the settlement stops short as it would have.
func (st *settlement) documentRemoved(ctx context.Context, q Queryer) bool {
	var removed bool
	if err := q.QueryRow(ctx, documentRemovedStatement, st.room).Scan(&removed); err != nil {
		return false
	}
	return removed
}

// readLocked is what the pass reads once its transaction holds the document's owner lock: the
// latest version, the document's cursor lock, the gate the room's instance passes, the room's
// updates through the pass's read stored, the update cursor its version would record, and the
// authors that version would credit. It answers settleOn where the pass goes on, and the step
// and outcome the pass ends on otherwise.
func (p *settlementPass) readLocked(ctx context.Context, parent context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	latest, err := latestVersion(ctx, p.tx, room)
	if err != nil {
		return settleDone, p.stoppedShort(err)
	}
	p.latest = latest
	if err := s.lockSettlementCursor(ctx, p.tx, room); err != nil {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return settleDone, settleOutcome{}
		}
		return settleDone, p.stoppedShort(fmt.Errorf("lock document cursor: %w", err))
	}
	if s.afterSettleLock != nil {
		s.afterSettleLock(room)
	}
	// The first read of a refused update's second order (roomRefusedUpdate): the room's
	// persistence worker met the append gate's refusal before this pass took the document's
	// advisory lock, so the room's tree holds an update no store write of this pass will meet.
	// Read once the lock is held, since from here to the commit no append of the room is refused.
	if p.roomRefusedUpdate() {
		return settleDone, p.endOnRefusedUpdate(p.slots...)
	}
	head, err := currentUpdateCursor(ctx, p.tx, room)
	if err != nil {
		return settleDone, p.stoppedShort(err)
	}
	// The gate, before this settlement writes anything. The room it holds must stand at that
	// head: with two envoy-dispatch processes on one Postgres, an update the other process
	// appended can be stored before this room's catch-up has taken it in, and settling from it
	// corrupts three things at once. Its version would record a doc_update_version at or past that
	// head over text that is missing the other process's update, so contentChangedSinceVersion
	// then reports nothing past that cursor and those words are never versioned;
	// clearSettlementPending would delete the one durable record that the work is still owed; and
	// reconcileAskBlocks would retract every open ask whose block this stale tree cannot see.
	//
	// So a room behind the head is caught up here, under the document's advisory lock this
	// transaction holds, reading through that transaction (catchUpRoom): no append can land
	// meanwhile, so the catch-up always brings the room to that head, and the settlement proceeds.
	// The rows it applies move the instance's cursor past the take, so this pass reads the room
	// again (settleMoved) and renders the other process's update with this one's. A history
	// replaced under the room is the one case catch-up cannot mend: the transaction rolls back,
	// which keeps doc_settlements_pending and releases the lock the room's close needs, and the
	// room is closed and reloaded (reloadReplacedRoom).
	//
	// The cursor is the one this settlement marked its context with, which its repairs' appends
	// are gated on (appendingCursor), so the two can never disagree.
	if p.cursor != nil && p.cursor.heldThrough() != head {
		outcome, err := s.catchUpRoom(ctx, p.tx, room, p.doc)
		switch {
		case err != nil:
			return settleDone, p.stoppedShort(err)
		case outcome == catchUpGone:
			// The room no longer holds the instance this settlement took, or a close holds it. As
			// in take, the retry this pass stops short into decides by the room's generation:
			// after a close that moved it, the document is left to what that close arranged; after
			// one that did not, the retry arms the settlement again, and that settlement, once this
			// pass's rollback has let the close finish, loads the room at the head.
			return settleDone, p.stoppedShort(fmt.Errorf("catch the room up to the durable head: %w", errRoomReplaced))
		case outcome == catchUpReplaced:
			// The rollback releases the document's advisory lock, which the room's close needs: a
			// close flushes the room's persistence, whose appends take that lock.
			_ = p.tx.Rollback(ctx)
			refusal := fmt.Errorf("%w: room %s holds the document through update %d, and the stored history was replaced under it (head %d)",
				ErrRoomBehindHead, room, p.cursor.heldThrough(), head)
			if parent.Err() != nil {
				return settleDone, p.stoppedShort(refusal)
			}
			s.reloadReplacedRoom(parent, room, state, p.generation, p.doc, refusal)
			return settleDone, settleOutcome{next: settleReloaded}
		}
		if through := p.cursor.heldThrough(); through != head {
			return settleDone, p.stoppedShort(fmt.Errorf("%w: room %s holds the document through update %d after its catch-up, the stored document stands at %d",
				ErrRoomBehindHead, room, through, head))
		}
		// The rows the catch-up applied are another process's content change, which re-arms the
		// settlement of a room whose settlement is armed or running - this one is - and moves the
		// room's generation with it (rearmSettle, scheduleSettleLocked). This pass then writes no
		// version and clears no pending settlement: the settlement it armed reads the caught-up
		// room and versions every update, this process's and the other's. A shutdown's settlement
		// re-arms nothing, so it reads the room again itself (settleMoved below) and versions it
		// there and then, nothing about the room being left for the next process.
		state.mu.Lock()
		superseded := state.roomGeneration != p.generation
		s.unlockState(room, state)
		if superseded {
			return settleDone, settleOutcome{}
		}
	}
	// The room stands at the head; it must also still stand where it stood at the take. An update
	// stored since may not be in the tree this pass rendered, and versioned, that tree would
	// record the update's cursor over text without it. The rollback leaves the document as it
	// was, and the next pass reads it again.
	if p.cursor != nil && p.cursor.heldThrough() != p.taken.heldThrough {
		return settleMoved, settleOutcome{}
	}
	// Whom a version of this pass would list, as the document's pending authors stand before it
	// stores anything. It stores nothing until the ask blocks are reconciled: the copy lock that
	// reconciliation can take (copiedAskSources, owedCopiesOf) comes before any pending-settlement
	// row, which each append writes (updateOwedStatement), in every transaction (lockProjectCopies).
	// A pass that goes on to write the version takes them again once the room's updates it holds
	// are stored (commitVersion).
	if err := p.takeCredit(ctx); err != nil {
		return settleDone, p.stoppedShort(err)
	}
	if s.afterSettleRead != nil {
		s.afterSettleRead(room)
	}
	return settleOn, settleOutcome{}
}

// takeCredit reads whom a version of this pass would list: the pending authors as this pass's
// lock holds them, the ones its queue has yet to write among them (transactionPendingAuthors) -
// no append or write changes them until its transaction ends - the latest edit source the
// document's pending-settlement row holds and whether it is the document's closed edit source
// (readSettlementEditSource), which every process's appends and commits write under this lock,
// and the document's latest person sender as the stored updates and commits recorded it
// (readLastPerson), which names who edited last once the room holds no in-flight credit that
// decides (latestEditSource). The in-flight credits are the ones the pass's take bounded
// (settlementAuthors). The ask blocks' sources the pass read stay (applyStamp). The version lists
// the pending authors and deletes them; a pass that writes no version leaves them for the next.
func (p *settlementPass) takeCredit(ctx context.Context) error {
	owed, err := transactionPendingAuthors(ctx, p.tx, p.room)
	if err != nil {
		return err
	}
	recorded, recordedKept, err := readSettlementEditSource(ctx, p.tx, p.room)
	if err != nil {
		return err
	}
	durableLatest, err := readLastPerson(ctx, p.tx, p.room)
	if err != nil {
		return err
	}
	sources := p.credit.askSources
	p.state.mu.Lock()
	p.credit = settlementAuthors(p.state, owed, recorded, recordedKept, durableLatest, p.taken)
	p.state.mu.Unlock()
	p.credit.askSources = sources
	return nil
}
