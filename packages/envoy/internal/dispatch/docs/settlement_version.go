package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// now reads differently from the latest one and its rendering reads back (readsBack), and commits
// the transaction all of that and the settlement's stored updates are in.
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
	// An issue room versions each of its documents on its own (roomFragments): a version of each
	// document whose fragment reads differently from that document's latest version, the tree
	// artifact's own fragment among them, settledVersion being the tree's. Every rendering the
	// settlement would store as a version reads back first (readsBack), and one that does not
	// writes no version of its own document, while in an issue room every other document whose
	// rendering reads back writes its version (holdUnreadableFragments). refused says one did not.
	var fragments []roomFragment
	refused := false
	if len(st.layout) > 0 {
		if fragments, err = st.roomFragments(ctx, contentChanged); err != nil {
			return settleDone, st.abandon(err)
		}
		refused = st.holdUnreadableFragments(fragments)
		versioning = slices.ContainsFunc(fragments, func(fragment roomFragment) bool { return fragment.changed })
		if fragments[0].changed {
			settledVersion = fragments[0].latest.number + 1
		}
	} else if versioning && !st.readsBack(room, markdown) {
		versioning, refused = false, true
	} else if versioning {
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
	// A settlement one of whose renderings did not read back (refused) has not settled what it
	// read: the document that rendering is of writes no version, so the row stays, with its latest
	// edit source, and the document stays owed. The row is the room's, so in an issue room every
	// document of the room keeps it, and the next settlement settles the whole room again, which
	// writes no version of a document whose markdown is unchanged; the resumption arms that
	// settlement where no edit does (RunSettlementResumption).
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
	switch {
	case refused:
		// The row stays, and the document with it owed.
	case st.issueClosed && kept == nil && st.savedNothing(versioning):
		if err := clearSettlementPendingRow(ctx, tx, room); err != nil {
			return settleDone, st.abandon(err)
		}
	default:
		if err := clearSettlementPending(ctx, tx, room, kept); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	// The version a repair, an invalid block or a retraction names is the one its block's document
	// stands at once this settlement commits: the document's own, or, in an issue room, the
	// version of the document whose fragment holds the block (blockVersions). documents is that
	// document of each block of an issue room's settled tree (roomLayout.blockDocuments), which
	// the room's pass and the ties of the asks it opened read too; nil for a document that is its
	// own room.
	versionOf := func(string) int { return settledVersion }
	var documents map[string]string
	if len(st.layout) > 0 {
		documents = st.layout.blockDocuments(room, st.versioned, st.versionedCounts)
		if versionOf, err = st.blockVersions(ctx, fragments, documents); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	if err := st.reconciliation.nameVersion(ctx, tx, room, st.owner, versionOf); err != nil {
		return settleDone, st.abandon(err)
	}
	// The copies among the document's ask blocks and the readability of the asks whose blocks
	// this settlement read are stated in the name the version itself carries (versionActor), at
	// one time (stateCopiesAndReadability).
	at := time.Now().UTC()
	// written is the version of each document the pass over the versions this settlement writes
	// covers, which the ties of the asks it opened record (writeOpenedAskTies, below); nil where it
	// writes none.
	var written refs.Versions
	var keptEditor *model.Actor
	if st.credit.kept {
		keptEditor = &st.credit.actor
	}
	if versioning && len(st.layout) > 0 {
		versions, writeErr := st.commitRoomVersions(ctx, fragments, documents, keptEditor, refused)
		if writeErr != nil {
			if len(st.slots) == 0 && errors.Is(writeErr, ErrDocNewerContent) && skipUnreadableSettlement(room, writeErr) {
				return settleDone, settleOutcome{}
			}
			return settleDone, st.abandon(writeErr)
		}
		written = versions
		// The copies and the readability, once the room's blocks are planned, under the last
		// version event the room's versions appended, right after it.
		if err := st.stateCopiesAndReadability(ctx, at, graphstore.LatestEvent); err != nil {
			return settleDone, st.abandon(err)
		}
	} else if versioning {
		// A person's edit in the room that took an answered ask's block out of the document is
		// that person's removal, which the version's index pass states on the ask's node, as the
		// same edit over HTTP records it (holdRoomRemovals). Only an answered ask the
		// reconciled tree held no block of can be one, or any where the version is a later read.
		if st.reconciliation.answeredGone || st.versioned != st.reconciled {
			if err := holdRoomRemovals(ctx, tx, room, st.versioned, st.credit.authors); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		// The version is written with no deferral mark (versionWrite.deferLarge unset), so its
		// blocks are indexed in this transaction whatever its size: this index pass is one of the
		// passes that close a document left owed its block index, and claimCopies
		// (stateCopiesAndReadability, below) then reads the nodes of this version's ask blocks
		// from the block index as the pass leaves it, passing over a block with no node.
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
		written = refs.OneVersion(room, result.version.Number, result.version.CreatedAt)
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
	// before the ask.opened that names the targets they moved is appended. An issue room any of
	// whose fragments was refused leaves them for its next index pass, as it leaves its index.
	if !(refused && len(st.layout) > 0) {
		// An issue room's ties record each block's own document's version: the one this
		// settlement wrote of it (written), else its latest.
		var versions refs.Versions
		switch {
		case written != nil:
			versions = written
		case len(st.layout) > 0:
			versions = latestFragmentVersions(fragments)
		default:
			versions = refs.OneVersion(room, settledVersion, st.latest.CreatedAt)
		}
		if err := st.reconciliation.writeOpenedAskTies(ctx, tx, room, versions, documents, st.versioned, offsets); err != nil {
			return settleDone, st.abandon(err)
		}
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
	// An issue room's write can move the record of a query whose document it writes no version of:
	// a requirement written into the Verification document's fragment moves the spec's record and no
	// byte of the spec. Such a version is no part of this transaction: its write phase marks the
	// issue's queries owed last, after the events and the tuples it appends (queueSettledQueryMark),
	// and the drain writes the versions once the settlement has committed (DrainOwedQueries).
	// This also covers a settlement that opens an ask without versioning its fragment: the ask's
	// node changes the decisions query even if that settlement writes no Markdown version.
	marked := false
	if len(st.layout) > 0 && st.owner.IssueKey != nil {
		if err := st.queueSettledQueryMark(ctx, *st.owner.IssueKey, caused, &marked); err != nil {
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
	// The fill records the record digests this settlement's versions left owed (fillOwed), and
	// the drain writes the versions of the queries it left owed, each in a transaction of its own
	// before its events are published, and what they appended is published after them, which it
	// follows in the log; a fill or a drain that fails leaves its work to the block-index tick.
	filled := s.fillOwed(ctx, ledger.takeFills())
	var drained []model.Event
	if marked {
		drained = s.DrainOwedQueries(ctx, *st.owner.IssueKey)
	}
	for _, event := range st.published {
		s.events.Publish(event)
	}
	for _, event := range filled {
		s.events.Publish(event)
	}
	for _, event := range drained {
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
	// reopen writes or marks owed. An issue room's sweep reads each fragment's marks against what
	// its own document records.
	if !st.issueClosed {
		s.sweepUnrecordedMarks(room, st.layout, st.versioned, st.versionedCounts)
	}
	return settleDone, settleOutcome{}
}

// queueSettledQueryMark queues, last in the settlement's write phase, the mark of the queries of
// the issue issueKey owed a version (QueueQueryMark), caused by the latest event the settlement
// appended (graphstore.LatestEvent) and the latest tuple it appended, whose writer, the
// settlement's version actor, the drain names its versions after. The mark's sequence is the last
// one this session drew, so it is queued only where the write phase appends a tuple before it: the
// pass over the room's versions states one (roomTuples, commitRoomVersions), or what the events the
// settlement appends cause holds one (caused, causedByEvent), read when the step runs, after
// appendEvents' own step read it. A settlement that appends no tuple moves no query's record.
// marked says, once the write phase has run, whether the issue has converted and so took the mark.
func (st *settlement) queueSettledQueryMark(ctx context.Context, issueKey string, caused map[int][]*graphstore.Uncaused, marked *bool) error {
	return write(ctx, st.tx, func(b *store.WriteBatch) error {
		states := st.roomTuples
		for _, stated := range caused {
			for _, held := range stated {
				states = states || len(held.Tuples) > 0
			}
		}
		if states {
			QueueQueryMark(b, issueKey, graphstore.LatestEvent, marked)
		}
		return nil
	})
}

// roomVersion is the latest markdown version a document of an issue room holds, as
// readRoomVersions reads it: its number, 0 for a document that holds none, its markdown and its
// row's created_at, the room update cursor it covers, the document's name, and the renderer
// version that wrote that markdown (renderer_version), nil where no write stamped one, as a
// document's own latest version carries it (latestVersionRow).
type roomVersion struct {
	number          int
	markdown        string
	at              time.Time
	name            string
	cursor          int64
	rendererVersion *int
}

// holds reports whether the version holds rendering, its document's fragment as this build
// renders it (renderFragment), by the rule a document's own latest version is held to
// (latestVersionRow.holds, holdsRendering): byte for byte, or, for a version another renderer
// version wrote or none stamped, as markdown this build reads as that rendering (readsAs). So a
// renderer change alone leaves a fragment's document unchanged, as it leaves a document that is
// its own room.
func (v roomVersion) holds(rendering string) bool {
	return holdsRendering(v.markdown, v.rendererVersion, rendering)
}

// readRoomVersions reads, in one statement through q, the latest markdown version of each of
// documents, beside it: the documents of an issue room, its tree artifact first
// (roomLayout.documents). Each document of a room keeps its own history (D6 of
// the issue-tree plan), and a block of the room records a version of its own document.
func readRoomVersions(ctx context.Context, q Queryer, documents []string) ([]roomVersion, error) {
	rows, err := q.Query(ctx, `
		select coalesce(v.number, 0), coalesce(v.markdown, ''), coalesce(v.created_at, a.created_at),
		       a.name, coalesce(v.doc_update_version, 0), v.renderer_version
		from unnest($1::uuid[]) with ordinality as d (id, position)
		join artifacts a on a.id = d.id
		left join lateral (
			select number, markdown, created_at, doc_update_version, renderer_version from artifact_versions
			where artifact_id = d.id and markdown is not null
			order by number desc limit 1
		) v on true
		order by d.position`, documents)
	if err != nil {
		return nil, fmt.Errorf("read the latest versions of the room's documents: %w", err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (roomVersion, error) {
		var version roomVersion
		err := row.Scan(&version.number, &version.markdown, &version.at, &version.name, &version.cursor, &version.rendererVersion)
		return version, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the latest versions of the room's documents: %w", err)
	}
	if len(versions) != len(documents) {
		return nil, fmt.Errorf("read the latest versions of the room's documents: %d of %d documents stand", len(versions), len(documents))
	}
	return versions, nil
}

// latestRoomVersions is readRoomVersions of documents as the transaction tx leaves them, for the
// index of a version its writer queued and has not written (indexDocumentInRoom): a document's
// latest version is the one tx queued where it queued one (the ledger's, which latestVersion
// answers with too), and a document tx creates (Ledger.RegisterPendingArtifact), whose row no read
// before tx's write phase sees, holds that version alone, or none. Every other document's is read
// from the table.
func latestRoomVersions(ctx context.Context, tx pgx.Tx, documents []string) ([]roomVersion, error) {
	stored := make([]string, 0, len(documents))
	for _, id := range documents {
		if _, created := pendingArtifact(ctx, id); !created {
			stored = append(stored, id)
		}
	}
	var read []roomVersion
	if len(stored) > 0 {
		var err error
		if read, err = readRoomVersions(ctx, tx, stored); err != nil {
			return nil, err
		}
	}
	ledger := queueFor(ctx, tx)
	versions := make([]roomVersion, len(documents))
	for index, id := range documents {
		if _, created := pendingArtifact(ctx, id); !created {
			versions[index], read = read[0], read[1:]
		}
		if ledger == nil {
			continue
		}
		if pending, queued := ledger.latest[id]; queued {
			versions[index].number, versions[index].markdown, versions[index].at = pending.Number, pending.markdown, pending.CreatedAt
			versions[index].cursor, versions[index].rendererVersion = pending.docUpdateVersion, pending.rendererVersion
		}
	}
	return versions, nil
}

// roomFragment is one fragment of an issue room as its settlement versions it (roomFragments):
// the document whose fragment it is, the tree artifact for its own, the part of the room's tree
// that fragment holds and its rendering, that document's latest version, and whether the
// settlement writes a version of it.
type roomFragment struct {
	document string
	tree     *pmdoc.Node
	markdown string
	offsets  []pmdoc.BlockOffset
	latest   roomVersion
	changed  bool
}

// roomFragments renders each fragment as a version of its document. Only a fragment its latest
// version does not hold (roomVersion.holds) can take a new version, so a version another renderer
// version wrote in its spelling of the same tree takes none. contentChanged says whether updates
// changed the room's text since the tree artifact's latest version. When that version is more
// recent than another fragment's, the other fragment can still owe edits, even with no content
// updates since the tree's version. Check those fragments against their own version's cursor.
// Retired documents are not in the layout (roomLayoutSQL).
func (st *settlement) roomFragments(ctx context.Context, contentChanged bool) ([]roomFragment, error) {
	documents := st.layout.documents(st.room)
	parts := st.layout.parts(st.versioned, st.versionedCounts)
	if parts == nil {
		return nil, fmt.Errorf("%w: the settled tree of room %s does not part into its %d fragments", ErrDocSchema, st.room, len(documents))
	}
	latest, err := readRoomVersions(ctx, st.tx, documents)
	if err != nil {
		return nil, err
	}
	fragments := make([]roomFragment, len(documents))
	for index, document := range documents {
		markdown, offsets, err := renderFragment(parts[index])
		if err != nil {
			return nil, err
		}
		changed := false
		if latest[index].number == 0 || !latest[index].holds(markdown) {
			changed = contentChanged
			if !changed && latest[index].cursor < st.latest.docUpdateVersion {
				changed, err = contentChangedSinceVersion(ctx, st.tx, st.room, latest[index].cursor)
				if err != nil {
					return nil, err
				}
			}
		}
		fragments[index] = roomFragment{
			document: document, tree: parts[index], markdown: markdown, offsets: offsets, latest: latest[index],
			changed: changed,
		}
	}
	return fragments, nil
}

// latestFragmentVersions is the latest version of the document of each of fragments that holds
// one, by document: what the room's ties record where the settlement writes no version, and what
// its pass covers before the versions it writes replace their documents' latest.
func latestFragmentVersions(fragments []roomFragment) refs.Versions {
	versions := refs.Versions{}
	for _, fragment := range fragments {
		if fragment.latest.number > 0 {
			versions[fragment.document] = refs.Version{Number: fragment.latest.number, At: fragment.latest.at}
		}
	}
	return versions
}

// blockVersions is, by a block's id, the version of the block's own document an issue room's
// settlement leaves it at (fragments): the version the settlement writes of a document whose
// fragment changed, else that document's latest. A block the settled tree holds is its fragment's
// document's (documents, roomLayout.blockDocuments), the tree artifact's for one of the room's own
// fragment; a block the tree no longer holds - an ask block whose retraction names the version it
// left in - is the document its issue_room_blocks row names, read in one statement while the
// room's index still holds that row, since this settlement's pass deletes it.
func (st *settlement) blockVersions(ctx context.Context, fragments []roomFragment, documents map[string]string) (func(blockID string) int, error) {
	numbers := make(map[string]int, len(fragments))
	for _, fragment := range fragments {
		numbers[fragment.document] = fragment.latest.number
		if fragment.changed {
			numbers[fragment.document]++
		}
	}
	var left []string
	for _, ask := range st.reconciliation.retracted {
		if ask.BlockID == nil {
			continue
		}
		if _, held := documents[*ask.BlockID]; !held {
			left = append(left, *ask.BlockID)
		}
	}
	// The blocks that left go into a map of their own: documents is the settled tree's alone, as
	// the room's pass and the ties read it.
	var leftDocuments map[string]string
	if len(left) > 0 {
		rows, err := st.tx.Query(ctx, `
			select r.block_id, r.document_id::text from issue_room_blocks r
			join unnest($2::text[]) as l (block_id) on block_id_key(r.block_id) = block_id_key(l.block_id) and r.block_id = l.block_id
			where r.tree_artifact_id = $1::uuid`, st.room, left)
		if err != nil {
			return nil, fmt.Errorf("read the documents of the blocks that left room %s: %w", st.room, err)
		}
		defer rows.Close()
		leftDocuments = make(map[string]string, len(left))
		for rows.Next() {
			var block, document string
			if err := rows.Scan(&block, &document); err != nil {
				return nil, fmt.Errorf("read the documents of the blocks that left room %s: %w", st.room, err)
			}
			leftDocuments[block] = document
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the documents of the blocks that left room %s: %w", st.room, err)
		}
	}
	return func(blockID string) int {
		if document, held := documents[blockID]; held {
			return numbers[document]
		}
		if document, held := leftDocuments[blockID]; held {
			return numbers[document]
		}
		return numbers[st.room]
	}, nil
}

// commitRoomVersions writes, in the settlement's transaction, one version of each document of the
// issue room whose fragment changed (fragments), every such fragment's rendering having read back
// (holdUnreadableFragments), each through writeVersionTx with the version's own approval move,
// anchors and edit notice, then indexes the room once under its tree artifact
// (indexRoom): each fragment's own rendering, the bytes its version holds, laid end to end in the
// room's order with its blocks' ranges (fragmentsText) and each block's document (documents,
// roomLayout.blockDocuments), at the version of each fragment's document
// - the one just written, or its latest - so each block records its own document's version and the
// pass clears the mark the version trigger set on each version it writes (refs.IndexBlocks'
// fence); then carries each version's approval once that pass has stated it, its writes before the
// versions' artifact.version events (R14), a restore's behind a row of the approval's digest
// (carryRestored).
//
// If a fragment did not read back (refused), the room's index would label live blocks with a
// version that does not hold them. Leave the index owed instead. The new version rows keep the
// marks their insert triggers set and take no render time until a pass can index the whole room.
// The pass does not state any blocks, but the readable versions still take their nodes and
// events, and their approvals can carry.
//
// Each version's node follows the pass's block nodes, and each
// version's artifact.version event names its own document and causes what the pass stated of
// that document's blocks, with the version's node, stamping the links its blocks inserted at that
// version (stampVersionEvent), each queued right after that event's append (graphstore.LatestEvent,
// causeRoomFacts); a document of the room this settlement writes no version of has its facts
// caused by the event that recorded its latest version, and so has the tree artifact, whose own
// fragment and references belong to no other document (roomFactsByDocument). The version events,
// their stamps and the causes are steps of the settlement's write phase (write), queued in that
// order, whose last event appended is the last version's, the cause of what the settlement goes
// on to state of the copies.
//
// Each version lists the authors of the updates that changed its own document's fragment, which
// the room's credit holds by document (settlementVersionCredit.documents), and names its actor and
// the latest editor its edit notice names by that list (documentActor, documentLatest): nobody is
// credited with a document whose fragment no update of theirs changed. Its commit deletes the
// pending authors it lists; those of a document it writes no version of stay for that document's
// next version.
func (st *settlement) commitRoomVersions(ctx context.Context, fragments []roomFragment, documents map[string]string, keptEditor *model.Actor, refused bool) (refs.Versions, error) {
	s, room, tx := st.service, st.room, st.tx
	// versions is the version of each document the room's pass covers, and inserted those of them
	// this settlement writes, whose nodes it writes after the pass.
	versions, inserted := latestFragmentVersions(fragments), refs.Versions{}
	type written struct {
		fragment roomFragment
		result   versionWriteResult
		actor    model.Actor
	}
	var writes []written
	listed := make(map[string][]model.Actor, len(fragments))
	for _, fragment := range fragments {
		if !fragment.changed {
			continue
		}
		actor := st.credit.documentActor(fragment.document)
		// Written with no deferral mark (versionWrite.deferLarge unset), and indexed by the room's
		// one pass below (indexRoom), whatever the room's size, unless a fragment is held back.
		// claimCopies (stateCopiesAndReadability) reads the ask blocks from the index as the pass
		// leaves it, passing over a block with no node.
		result, err := s.writeVersionTx(ctx, tx, fragment.document,
			renderedTree{tree: fragment.tree, markdown: fragment.markdown, offsets: fragment.offsets}, actor, &versionWrite{
				authors:          st.credit.documents[fragment.document],
				docUpdateVersion: &st.snapshotCursor,
				closedIssue:      st.issueClosed,
				settlement:       true,
				keptEditor:       keptEditor,
				roomIndexed:      true,
			})
		if err != nil {
			return nil, err
		}
		versions[fragment.document] = refs.Version{Number: result.version.Number, At: result.version.CreatedAt}
		inserted[fragment.document] = versions[fragment.document]
		writes = append(writes, written{fragment: fragment, result: result, actor: actor})
		listed[fragment.document] = st.credit.listed[fragment.document]
	}
	var index roomIndex
	var err error
	if !refused {
		markdowns := make([]string, len(fragments))
		offsets := make([][]pmdoc.BlockOffset, len(fragments))
		for at, fragment := range fragments {
			markdowns[at], offsets[at] = fragment.markdown, fragment.offsets
		}
		markdown, laid := fragmentsText(markdowns, offsets)
		index, err = s.indexRoom(ctx, tx, room, versions, inserted, false, markdown, offsetBlocks(laid, documents),
			st.versioned, &st.reconciliation.sources)
		if err != nil {
			return nil, err
		}
	}
	if index.facts == nil {
		index.facts = make(map[string]graphstore.Uncaused, len(writes))
	}
	// Each version's approval carry runs once the room's pass has stated it, its writes before the
	// artifact.version events below (R14): the review it inserts pins the digest of the version's
	// stored markdown while the version's record digest is owed, and the fill after the commit
	// repins it to the record (fillOwedDigests); a carry to a version that restores the approved
	// text writes behind a row of the approval's digest the write phase holds to the version's
	// record (carryRestored). Each version's move ran in its write, before the pass, so an open
	// request names the version by now. A closed issue's version carries nothing. The record case
	// weighs each version by the facts of the room's whole pass (roomPassRecord): a block written
	// into one document's fragment can move another document's query.
	if !st.issueClosed {
		record := roomPassRecord(index)
		for _, write := range writes {
			document, version, fragment := write.fragment.document, write.result.version, write.fragment
			if err := s.carryApproval(ctx, tx, document, version,
				renderedTree{tree: fragment.tree, markdown: fragment.markdown, offsets: fragment.offsets}, decisionStepCases{}, record, eventSink(ctx)); err != nil {
				return nil, err
			}
		}
	}
	for _, write := range writes {
		node, err := WriteVersionNode(ctx, tx, write.result.owner, write.result.version.Number, write.actor, write.result.version.CreatedAt)
		if err != nil {
			return nil, err
		}
		index.facts[write.fragment.document] = graphstore.Joined(index.facts[write.fragment.document], node)
	}
	// Each version lists the pending authors this settlement read of its document, so its commit
	// deletes them.
	if err := deleteRoomPendingAuthors(ctx, tx, listed); err != nil {
		return nil, err
	}
	if s.afterSettleVersionWrite != nil {
		if err := s.afterSettleVersionWrite(room); err != nil {
			return nil, err
		}
	}
	// Each version opens or moves its own document's edit notice, as a document's settlement does
	// (commitVersion), and a closed issue's deletes it.
	for _, write := range writes {
		document := write.fragment.document
		if st.issueClosed {
			if err := writeExec(ctx, tx, "delete the closed issue's document's edit notice", dropEditNotice, document); err != nil {
				return nil, err
			}
		} else if err := s.recordEditNotice(ctx, tx, document, write.result.version.Number, st.credit.documents[document], st.credit.documentLatest(document)); err != nil {
			return nil, err
		}
	}
	// The events each document's facts and stamps take: a written document's own artifact.version,
	// appended in the step below right before them (graphstore.LatestEvent), in the order the
	// versions were written, and then the stored event that recorded each other document's latest
	// version, in id order.
	events := make(map[string]int64, len(versions))
	order := make([]string, 0, len(versions))
	for _, write := range writes {
		events[write.fragment.document] = graphstore.LatestEvent
		order = append(order, write.fragment.document)
	}
	if !refused {
		for _, document := range slices.Sorted(maps.Keys(versions)) {
			if _, own := events[document]; own {
				continue
			}
			if events[document], err = versionEventID(ctx, tx, st.owner, document, versions[document].Number); err != nil {
				return nil, err
			}
			order = append(order, document)
		}
	}
	facts := roomFactsByDocument(index, roomReferencesDocument(room, versions, events, order))
	// The pass's facts are appended after the version events of its own writes, so a pass that
	// writes a version and states a tuple leaves this session's latest event and latest tuple its
	// own, which the mark of the issue's queries reads (queueSettledQueryMark).
	st.roomTuples = false
	for _, stated := range facts {
		st.roomTuples = st.roomTuples || len(writes) > 0 && len(stated.Tuples) > 0
	}
	// The room's references and its blocks' links name what the pass moved, and the last event
	// carries them, as a document's one event does.
	planned := make([]model.Event, len(writes))
	for position, write := range writes {
		moved := model.ReferenceChanges{}
		if position == len(writes)-1 {
			moved = index.changes
		}
		document := write.fragment.document
		planned[position] = model.Event{
			IssueKey: st.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    write.actor,
			Payload:  ArtifactVersionEventPayload(document, write.fragment.latest.name, write.result.version, nil, moved),
		}
		if st.owner.IssueKey == nil {
			planned[position].ArtifactID = &document
		}
	}
	sink := eventSink(ctx)
	for position, own := range writes {
		document := own.fragment.document
		event := planned[position]
		if err := write(ctx, tx, func(b *store.WriteBatch) error {
			if err := s.events.QueueAppend(b, event, sink); err != nil {
				return fmt.Errorf("append the version event of %s: %w", document, err)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if !refused {
			if err := write(ctx, tx, func(b *store.WriteBatch) error {
				stampVersionEvent(b, room, document, versions[document].Number, versions[document].At, graphstore.LatestEvent)
				return nil
			}); err != nil {
				return nil, err
			}
		}
		if err := causeRoomFacts(ctx, tx, room, facts, events, []string{document}); err != nil {
			return nil, err
		}
	}
	for _, document := range order[len(writes):] {
		if err := write(ctx, tx, func(b *store.WriteBatch) error {
			stampVersionEvent(b, room, document, versions[document].Number, versions[document].At, events[document])
			return nil
		}); err != nil {
			return nil, err
		}
	}
	var rest []string
	for _, document := range slices.Sorted(maps.Keys(facts)) {
		if _, own := inserted[document]; !own {
			rest = append(rest, document)
		}
	}
	if err := causeRoomFacts(ctx, tx, room, facts, events, rest); err != nil {
		return nil, err
	}
	return versions, nil
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
