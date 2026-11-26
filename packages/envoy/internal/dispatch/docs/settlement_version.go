package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stateCopiesAndReadability plans the copied_from of the nodes of the document's ask blocks
// (claimCopies) and the readability of the asks whose blocks this settlement read
// (stateInvalidReasons), both in the name the version carries (versionActor) at at, and queues
// their statements under cause as one step (write): the version's event, which the step follows
// right after its append (graphstore.LatestEvent), or graphstore.NoEvent for a settlement that
// writes no version. It runs once the version's blocks are planned, so the copies read the nodes
// the block index leaves (heldBlockIndex).
func (st *settlement) stateCopiesAndReadability(ctx context.Context, at time.Time, cause int64) error {
	who := st.credit.versionActor()
	copies, err := st.reconciliation.claimCopies(ctx, st.tx, st.room, who, at)
	if err != nil {
		return err
	}
	readability, err := st.reconciliation.stateInvalidReasons(ctx, st.tx, who, at)
	if err != nil {
		return err
	}
	if copies == nil && readability == nil {
		return nil
	}
	return write(ctx, st.tx, func(b *store.WriteBatch) error {
		if copies != nil {
			if err := copies(b, cause); err != nil {
				return err
			}
		}
		if readability != nil {
			return readability(b, cause)
		}
		return nil
	})
}

// commitVersion renders the document the settlement settled, writes its version when the document
// now reads differently from the latest one, and commits the transaction all of that and the
// settlement's stored updates are in.
//
// A settlement of a closed issue's document wakes nobody: the text it saves was in the live
// document already, often for long, so an edit notice would report nothing new. Its version opens
// no edit notice and deletes the one the document holds (document_edit_notices), moves no
// approval request and carries no approval (versionWrite.closedIssue), and the settlement sweeps
// no marks after it commits. The version still lists the people whose edits it saves, and its
// artifact.version event names the version's actor (versionActor): the actor its credit names, or
// SettlementActor where that credit is only the kept editor (below); an unnamed version is quiet
// whoever its actor is (events.Quiet). The credited actor outlives the pending row the commit
// deletes, for the settlement after the reopen to name, which wakes whom the edit's own settlement
// would have woken on the open issue (clearSettlementPending).
//
// The editor so kept is credited with the text that editor wrote and with no version that adds
// none of it. A version whose credit is only that editor (settlementVersionCredit.kept) - one of
// the reopen's ask-state write, an ask repair's sweep, a rebuild or a copy marked owed, each
// naming no editor of its own - names SettlementActor on its artifact.version event and as its
// write's actor (versionActor). On an open issue its derived events, and the move of an approval
// request that names a version from before the closed issue's versions, still name that editor
// (versionWrite.keptEditor); on a closed issue every derived event names SettlementActor
// (planRepairs).
func (st *settlement) commitVersion(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state, tx, ledger := st.service, st.room, st.state, st.tx, st.ledger
	// read is the settlement's private copy of the document (readSettlementTree), and nothing but
	// planRepairs changes it in place, which records each change it makes (changedTree).
	// versioned is another tree once a stamp replaced the read or a write read the document again.
	// So the version takes the rendering of the read when it is that tree, unchanged.
	markdown, offsets, err := st.markdown, st.offsets, st.renderErr
	reused := st.versioned == st.read && !st.reconciliation.changedTree()
	if s.beforeSettleVersionRender != nil {
		s.beforeSettleVersionRender(room, st.read, reused)
	}
	if !reused {
		markdown, offsets, err = renderTreeWithOffsets(st.versioned)
	}
	if err != nil {
		if len(st.slots) == 0 && skipUnreadableSettlement(room, err) {
			return settleDone, settleOutcome{}
		}
		return settleDone, st.abandon(err)
	}

	// The second read of a refused update's second order (roomRefusedUpdate), beside the reads of
	// a stopping service and a failed room, after the last store write of this settlement was gated
	// (flushThrough and storeRepair decide in the read phase what the write phase stores) and
	// before its commit: what it would commit is rendered from a tree that holds an update the
	// room's closed issue refused.
	if st.roomRefusedUpdate() {
		return settleDone, st.endOnRefusedUpdate()
	}
	state.mu.Lock()
	// A settlement that wrote into the room commits what it wrote even when the document has moved
	// since its read: the room and its peers hold those updates, and dropped here they would never
	// reach the store. Its version is the document as it stood at its last read, after the repairs,
	// every update of which this transaction stores; a move made after that read is in a later
	// version.
	if s.stopping.Load() || state.failed != nil || (state.roomGeneration != st.generation && len(st.slots) == 0) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, st.slots...)
		return settleDone, settleOutcome{}
	}
	s.unlockState(room, state)

	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical and credited to nobody (LEGION-273, where it staled an approval pinned to what
	// an agent had just written; carryApproval now carries that approval to such a version
	// instead, an event nothing in the log would explain). Its tree writes - stamping block ids,
	// restoring an ask block's server-owned attributes - move the stored Proof state and often
	// render the same markdown, and a version for one of those would repeat the version before it
	// too (LEGION-229 requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since. The second
	// half is whether the latest version holds this rendering (latestVersionRow.holds): byte for
	// byte, or, for a version another renderer version wrote or none stamped, as markdown that
	// reads as it. So a renderer change leaves no version behind it either where an edit was made
	// and undone since, which changed the content and changed it back: the version holds the
	// same tree in the spelling its renderer wrote.
	contentChanged, err := contentChangedSinceVersion(ctx, tx, room, st.latest.docUpdateVersion)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	versioning := contentChanged && !st.latest.holds(markdown)
	// A retired document (RetireDocument) takes no new version: no list holds it and no reader
	// looks for text in it. Whether it is retired is the read beside the owner's lock this
	// settlement took (lockArtifactOwner, artifactOwner.Retired), the lock the retirement takes
	// too, so a retirement either committed before that read or commits after this settlement.
	// Typing a room took before the retirement closed it, or took in a room the retirement's close
	// missed (ServeHTTP), is still stored - this settlement commits the updates it read, as every
	// settlement does - and logged, so it is kept rather than dropped, but no version of the
	// retired document holds it.
	if versioning && st.owner.Retired {
		slog.Warn("dispatch: settlement stores the updates of a retired document and writes no version of it",
			"room", room, "latest_version", st.latest.Number)
		versioning = false
	}
	settledVersion := st.latest.Number
	if versioning {
		settledVersion++
	}
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit. It goes before the first event this
	// settlement appends. A transaction marking this document owed as a copy (owedCopiesOf) holds
	// its row until it commits, and this settlement then waits for it holding its owner lock and
	// room lock, which that transaction never takes; nor does it take what this settlement goes on
	// to lock, its events locking only this document's owner and owner row. The pending authors
	// stay unless this settlement's version lists them (deleted below): a settlement that writes no
	// version lists no one.
	//
	// The row's latest edit source goes with it, and the settlement after a reopen still names it:
	// before a closed issue's document settled, the row stood through the closed period, and that
	// settlement named its editor on its events and as the mover of an approval request. So on a
	// closed issue the editor this settlement credits is kept past the row (clearSettlementPending,
	// doc_closed_edit_sources), for the reopen's mark to write back into the row it inserts
	// (settlementPendingStatement) and the reopen's move to name (FollowClosedIssueVersions). A
	// settlement on an open issue has named its editor itself, and one on a closed issue that
	// credits SettlementActor for the edits it saves names no editor, as the settlement after the
	// reopen, crediting the same edits, named none then: each forgets the editor an earlier
	// settlement kept. One on a closed issue whose credit is only the kept editor
	// (settlementVersionCredit.kept), its version adding none of their text, keeps that same
	// editor, so the reopen still finds the editor of the text the closed issue's versions hold.
	// So does one on a closed issue that saved nothing (savedNothing): it credits SettlementActor
	// only because it found nothing to credit, and its commit deletes the pending row alone,
	// whatever arming ran it after the settlement that kept the editor. The delete and the keep
	// are queued, as every write of this settlement is, for its write phase.
	var kept *model.Actor
	if st.issueClosed && !st.credit.actor.SameAs(SettlementActor) {
		kept = &st.credit.actor
	}
	if st.issueClosed && kept == nil && st.savedNothing(versioning) {
		if err := clearSettlementPendingRow(ctx, tx, room); err != nil {
			return settleDone, st.abandon(err)
		}
	} else if err := clearSettlementPending(ctx, tx, room, kept); err != nil {
		return settleDone, st.abandon(err)
	}
	if err := st.reconciliation.nameVersion(ctx, tx, room, st.owner, settledVersion); err != nil {
		return settleDone, st.abandon(err)
	}
	// The copies among the document's ask blocks and the readability of the asks whose blocks
	// this settlement read are stated in the name the version itself carries (versionActor), at
	// one time (stateCopiesAndReadability).
	at := time.Now().UTC()
	if versioning {
		var keptEditor *model.Actor
		if st.credit.kept {
			keptEditor = &st.credit.actor
		}
		// A person's edit in the room that took an answered ask's block out of the document is
		// that person's removal, which the version's index pass states on the ask's node, as the
		// same edit over HTTP records it (holdRoomRemovals). Only an answered ask the
		// reconciled tree held no block of can be one, or any where the version is a later read.
		if st.reconciliation.answeredGone || st.versioned != st.reconciled {
			if err := holdRoomRemovals(ctx, tx, room, st.versioned, st.credit.authors); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		result, writeErr := s.writeVersionTx(ctx, tx, room, renderedTree{tree: st.versioned, markdown: markdown, offsets: offsets}, st.credit.versionActor(), &versionWrite{
			authors:          st.credit.authors,
			capture:          &st.credit.capture,
			docUpdateVersion: &st.snapshotCursor,
			settled:          &st.reconciliation.sources,
			closedIssue:      st.issueClosed,
			settlement:       true,
			keptEditor:       keptEditor,
		})
		if writeErr != nil {
			// The version a human approved holds an element this build does not know: the version
			// is left, unwritten, to a build that reads it, as a tree holding one is.
			if len(st.slots) == 0 && errors.Is(writeErr, ErrDocNewerContent) && skipUnreadableSettlement(room, writeErr) {
				return settleDone, settleOutcome{}
			}
			return settleDone, st.abandon(writeErr)
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, tx, room, st.credit.capture); err != nil {
			return settleDone, st.abandon(err)
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		// A person's version opens or moves on the document's edit notice, and a closed issue's
		// deletes it (settledEditNotice).
		if err := s.settledEditNotice(ctx, tx, room, st.issueClosed, result.version.Number, st.credit.authors, st.credit.capture.latest); err != nil {
			return settleDone, st.abandon(err)
		}
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: st.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    st.credit.versionActor(),
			Payload:  ArtifactVersionEventPayload(room, st.owner.Name, result.version, nil, result.changes),
		}
		if st.owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		// What this settlement's block index stated waits for that event: its id is the cause
		// every node and tuple of the pass carries, and it exists only once the event's insert has
		// run, so it is stated right after it (appendEvents).
		if err := st.appendEvents(ctx, []model.Event{versionEvent}, map[int][]*graphstore.Uncaused{0: {&result.blocks}}); err != nil {
			return settleDone, st.abandon(err)
		}
		// The copies and the readability, once the version's blocks are planned, under that same
		// event, right after it.
		if err := st.stateCopiesAndReadability(ctx, at, graphstore.LatestEvent); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// An ask this settlement opened has its row and node only in this write phase, so the index of
	// the version this settlement writes, which reads the asks table, states none of its block's
	// shaped ties, and nor did the index of a version written before. They are written here, after
	// the rows and after that version's index, whether or not this settlement writes a version,
	// before the ask.opened that names the targets they moved is appended.
	if err := st.reconciliation.writeOpenedAskTies(ctx, tx, room, settledVersion, st.versioned, offsets); err != nil {
		return settleDone, st.abandon(err)
	}
	// The nodes and the follows of the asks this settlement opened, their blocks' ties, and what
	// each answer state it wrote states on its ask's node wait for the events that record them,
	// each stated right after its own (causedByEvent).
	caused, err := st.reconciliation.causedByEvent(st.reconciliation.events)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	if err := st.appendEvents(ctx, st.reconciliation.events, caused); err != nil {
		return settleDone, st.abandon(err)
	}
	// A settlement that writes no version states the copies and the readability under no event.
	if !versioning {
		if err := st.stateCopiesAndReadability(ctx, at, graphstore.NoEvent); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// The write phase: every statement this settlement decided on, in the order it made them.
	if err := ledger.runQueue(ctx, nil); err != nil {
		return settleDone, st.abandon(err)
	}
	st.published = ledger.events
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state, and the room updates the settlement stored marked stored (commitSettlement); a
	// settlement that writes no version takes out none. The room forgets the author of each ask
	// block the commit indexed under the same hold, so no reader that finds the block's ask row
	// committed then finds the room still owing its author.
	committed := authorCapture{state: state}
	if versioning {
		committed = st.credit.capture
	}
	if advanced, err := s.commitSettlement(ctx, tx, ledger, room, committed, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err != nil {
			return
		}
		st.credit.consumeAskAuthorsLocked(st.reconciliation.indexedAskBlocks)
		if state.roomGeneration == st.generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		return settleDone, st.commitFailed(advanced, fmt.Errorf("commit document settlement: %w", err))
	}
	st.finishSlots()
	for _, event := range st.published {
		s.events.Publish(event)
	}
	st.reconciliation.logCopiedBlocks(room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement, which
	// its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption should this
	// process stop first.
	for _, copy := range st.reconciliation.owedCopies {
		s.scheduleSettle(copy)
	}
	// A closed issue's settlement sweeps no marks: the unmark is an injection without the
	// owner-verified mark, which allowInject refuses on a closed issue. The document's next
	// settlement on the open issue sweeps them: at the reopen, one runs only for a document the
	// reopen writes or marks owed.
	if !st.issueClosed {
		s.sweepUnrecordedMarks(room, st.versioned)
	}
	return settleDone, settleOutcome{}
}

// settledEditNotice is what a settlement's version does to the document's edit notice, under the
// owner lock the settlement took first: the sweep takes the two in that order too
// (sendEditNotice). A person's version on an open issue opens or moves on the notice
// (recordEditNotice). A closed issue's version opens none, and deletes in that same order the
// notice a person's version opened before the close, due or held (holdEditNotice): neither a
// reopen inside its quiet time nor the first notice after the reopen, which would start from that
// row's from_version (openEditNotice), carries the edits this version saves. The version a joined
// transaction writes as the settlement would (SettleVersion) does the same, and a snapshot's,
// whose closedIssue is false.
func (s *Service) settledEditNotice(ctx context.Context, tx pgx.Tx, room string, closedIssue bool, version int, authors []model.Actor, latest *model.Actor) error {
	if closedIssue {
		return writeExec(ctx, tx, "delete the closed issue's document's edit notice", dropEditNotice, room)
	}
	return s.recordEditNotice(ctx, tx, room, version, authors, latest)
}
