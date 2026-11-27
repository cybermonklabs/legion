package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// RefsFormsLease names the lease one pass of the forms duty runs under
// (RunRefsFormsReconciliation), which drains its three queues, refs_forms_owed,
// refs_decisions_forms_owed and refs_decisions_forms_seed_owed. A previous build can hold the
// block index lease without draining these queues. It can also settle refs_forms_owed under
// its own refs-forms lease, and thereby enqueue work it cannot finish in the live queue.
// The new lease lets this build drain that work without sharing a period with the old duty.
const RefsFormsLease = "refs-forms-decisions"

// RefsFormsTick is what one pass of the forms drain did (ReconcileRefsForms).
type RefsFormsTick struct {
	// Texts is how many owed comments, asks and issue messages it re-indexed, and Documents how
	// many owed documents it indexed itself, each row settled in the transaction that indexed it.
	Texts     int
	Documents int
	// Gone is how many rows it settled for a document that is gone or holds no markdown version,
	// which owes no text.
	Gone int
	// LiveAhead is how many owed documents it left owed because their live text is ahead of
	// their latest version and differs from it (indexOwedDocument), those an earlier pass found
	// so and that are still as it found them included, Unreadable how many it left owed because a
	// fragment of their issue room did not read back (indexOwedRoom), counted as LiveAhead is, and
	// Unindexed how many it left owed because its own index did not run: another process indexed
	// the document first, or a write moved the document's latest version or the row's
	// enqueued_at meanwhile. A room is counted once a pass, whichever of its documents' rows the
	// pass lists, as the block index counts one.
	LiveAhead  int
	Unreadable int
	Unindexed  int
	// OwedTexts is what the pass's queue holds after the pass.
	OwedTexts int
	// Cursor is the row the next pass lists after (refs.ListOwedAfter): the last row this pass
	// took while its listing was full or it left documents for the next pass, and the start
	// again, the zero row, once a listing reaches the end of the queue.
	Cursor refs.Owed
	// Full reports a listed batch that was full, or documents the pass left for the next one, so
	// more may be waiting.
	Full bool
}

// RunRefsFormsReconciliation drains refs_forms_owed (refs.RefsFormsOwed, migrations
// 20261117025900-20261117025910): every comment, ask, issue message and document an older build
// indexed without the reference forms this build reads - an issue's tab addresses and a
// document's dashboard URLs - which that build's index deleted the edges of; and the two queues of
// the reading of an issue's Decisions page (migrations 20261126040000-20261126040600): the live
// queue, refs_decisions_forms_owed (refs.RefsDecisionsFormsOwed), of every source the release
// before this one indexed, and settled its row of the first queue for, without that reading, which
// that build's index deleted the edges of or wrote the retired decisions document's edge beside,
// and the bootstrap queue, refs_decisions_forms_seed_owed (refs.RefsDecisionsFormsSeedOwed), of
// every text already stored when the seed ran. A row stays owed until a build that reads those
// forms has indexed that text itself.
//
// A duty of its own, under its own lease (RefsFormsLease), rather than a batch of the block-index
// pass (RunBlockIndexReconciliation), which drains refs_grammar_owed: during a rolling update a
// period the older process served there would drain nothing of these queues. One tick at start,
// then every blockIndexStep while a batch of any queue is full and every
// BlockIndexReconcileInterval once none is, until ctx ends, as the block-index pass is paced
// (duty.RunLeasedAfterEachPass). A tick is one pass of each queue, the forms queue first and the
// bootstrap queue last, so a row the older release relays while the first pass runs is listed by
// the second. Each queue keeps the block index's two pieces of state across its passes: the keyset
// cursor each pass starts from, and the documents its passes found live ahead (formsLiveAhead),
// apart from the others', since a cursor is a row of one queue's order. The passes run on the one
// connection of the lease pool the loop reserves ("refs forms", cmd/dispatch's main), whichever of
// them was full or failed.
func (s *Service) RunRefsFormsReconciliation(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	drains := newFormsDrains()
	duty.RunLeasedAfterEachPass(ctx, leases, RefsFormsLease, BlockIndexReconcileInterval, func(ctx context.Context, _ time.Duration) time.Duration {
		return s.refsFormsTick(ctx, drains)
	})
}

// formsDrain is one queue's state across the passes of the forms duty: the queue, the cursor its
// next pass lists after, the documents its passes found live ahead, and the last pass it logged.
type formsDrain struct {
	queue     refs.OwedQueue
	cursor    refs.Owed
	liveAhead formsLiveAhead
	// logged is the last pass this drain logged: a pass that drained nothing and leaves the same
	// counts (a document whose live text stays ahead, left owed every pass) logs nothing.
	logged RefsFormsTick
	// oneShot says nothing writes the queue after its migration, so a pass that finds it empty is
	// the last: done is then set, and the tick reads the queue no more.
	// kept for one release: decisions-ref-repair
	oneShot bool
	done    bool
}

func newFormsDrain(queue refs.OwedQueue) *formsDrain {
	return &formsDrain{queue: queue, liveAhead: newFormsLiveAhead()}
}

// newFormsDrains is the drains of the forms duty, in the order a tick runs them: the forms queue,
// the live queue of the Decisions page's reading, and its bootstrap queue, which only the seed
// migration writes and which the duty stops reading once a pass finds it empty.
func newFormsDrains() []*formsDrain {
	// kept for one release: decisions-ref-repair
	seed := newFormsDrain(refs.RefsDecisionsFormsSeedOwed)
	seed.oneShot = true
	// kept for one release: decisions-ref-repair
	return []*formsDrain{newFormsDrain(refs.RefsFormsOwed), newFormsDrain(refs.RefsDecisionsFormsOwed), seed}
}

// refsFormsTick runs one pass of each drain that is not done, in order, and returns how long to
// wait before the next tick: blockIndexStep when any pass reported a full batch,
// BlockIndexReconcileInterval otherwise. A pass that fails or is full never keeps the passes after
// it from running; only the end of ctx does.
func (s *Service) refsFormsTick(ctx context.Context, drains []*formsDrain) time.Duration {
	wait := BlockIndexReconcileInterval
	for _, drain := range drains {
		// kept for one release: decisions-ref-repair
		if drain.done {
			continue
		}
		if s.formsDrainPass(ctx, drain) {
			wait = blockIndexStep
		}
		if ctx.Err() != nil {
			return BlockIndexReconcileInterval
		}
	}
	return wait
}

// formsDrainPass runs one pass of drain from its cursor, logs what it did unless it drained
// nothing and left the counts drain.logged holds, moves the drain's cursor, and reports whether
// the pass left more waiting (RefsFormsTick.Full). A one-shot drain whose pass ran without an error
// and left its queue empty is done, and logs that once.
func (s *Service) formsDrainPass(ctx context.Context, drain *formsDrain) bool {
	tick, err := s.ReconcileRefsForms(ctx, drain.queue, drain.cursor, drain.liveAhead)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		slog.Error("dispatch: refs forms reconciliation", "queue", drain.queue.Table(), "error", err)
	}
	if tick.Texts > 0 || tick.Documents > 0 || tick.Gone > 0 || tick.LiveAhead != drain.logged.LiveAhead ||
		tick.Unreadable != drain.logged.Unreadable || tick.Unindexed != drain.logged.Unindexed || tick.OwedTexts != drain.logged.OwedTexts {
		slog.Info("dispatch: refs forms index", "queue", drain.queue.Table(), "texts", tick.Texts,
			"documents", tick.Documents, "gone", tick.Gone, "live_ahead", tick.LiveAhead,
			"unreadable", tick.Unreadable, "unindexed", tick.Unindexed, "owed_texts", tick.OwedTexts)
		drain.logged = tick
	}
	drain.cursor = tick.Cursor
	// kept for one release: decisions-ref-repair
	if drain.oneShot && err == nil && tick.OwedTexts == 0 && !tick.Full {
		drain.done = true
		slog.Info("dispatch: refs forms index found its bootstrap queue empty, which only its migration writes, and reads it no more",
			"queue", drain.queue.Table())
	}
	return tick.Full
}

// ReconcileRefsForms lists at most blockIndexItemBatch rows of one forms queue past after, oldest
// first (refs.ListOwedAfter). A comment, an ask or an issue message goes through refs.ReindexOwed.
// A document is indexed at its latest version (drainFormsDocument), or as part of its issue room,
// once per room per pass (drainFormsRoomDocument). A complete index reads every form and settles
// the rows it indexed in all forms queues in its own transaction; a settle without indexing is
// confined to the row listed by this queue. The pass stops at the document batch bound; the next
// pass starts there after blockIndexStep.
//
// The rows are listed by keyset, as the block index lists its documents, so a row this drain
// leaves owed never holds back the rows past it: a document whose live text is ahead, one whose
// issue room holds a fragment that does not read back, one holding an element this build does not
// know (ErrDocNewerContent), or a row that fails for a reason of its own. The cursor moves past
// every row it takes, drained or not, then goes back to the start once a listing is not full.
// liveAhead remembers the documents earlier passes over this queue found live ahead or unreadable,
// so they are counted again without indexing while their version and settlement mark stand.
func (s *Service) ReconcileRefsForms(ctx context.Context, queue refs.OwedQueue, after refs.Owed, liveAhead formsLiveAhead) (RefsFormsTick, error) {
	// A caller outside the loop can pass a zero memo, as the block index's can pass none: a
	// throwaway one costs no more than reading again the documents this one pass finds live ahead.
	if liveAhead.documents == nil {
		liveAhead = newFormsLiveAhead()
	}
	tick := RefsFormsTick{Cursor: after}
	owed, err := refs.ListOwedAfter(ctx, s.store.Pool, queue, after, blockIndexItemBatch)
	if err != nil {
		return tick, err
	}
	tick.Full = len(owed) == blockIndexItemBatch
	taken := owed
	documents := 0
	// An issue room is drained whole, once a pass, whichever of its documents' rows the pass lists
	// (drainFormsRoomDocument): rooms holds how each room this pass indexed fared, and a room is
	// counted once, as the block index counts one (ReconcileBlockIndex).
	rooms := map[string]owedOutcome{}
	counted := roomCounter{}
rows:
	for index, source := range owed {
		switch source.Kind {
		case "comment", "ask", "message":
			indexed, err := refs.ReindexOwed(ctx, s.store.Pool, source, s.serverURL)
			switch {
			case ctx.Err() != nil:
				return tick, ctx.Err()
			case err != nil:
				slog.Error("dispatch: refs forms index skipped text it could not index",
					"queue", queue.Table(), "kind", source.Kind, "id", source.ID, "error", err)
			case indexed:
				tick.Texts++
			}
		case "artifact":
			document, err := s.latestDocumentVersion(ctx, source.ID)
			remembered, still := s.stillLiveAhead(liveAhead.documents, document)
			switch {
			case ctx.Err() != nil:
				return tick, ctx.Err()
			case err != nil:
				slog.Error("dispatch: refs forms index skipped a document it could not index",
					"queue", queue.Table(), "artifact", source.ID, "error", err)
			case still:
				if counted.counts(document) {
					drained := formsDocumentLiveAhead
					if remembered.unreadable {
						drained = formsDocumentUnreadable
					}
					drained.count(&tick)
				}
				liveAhead.rows[document.id] = source
			case documents == blockIndexDocumentBatch:
				// This document and the rows after it wait for the next pass, which starts here.
				taken, tick.Full = owed[:index], true
				break rows
			default:
				documents++
				drained, err := s.drainFormsDocument(ctx, source, document, liveAhead, rooms)
				switch {
				case ctx.Err() != nil:
					return tick, ctx.Err()
				case errors.Is(err, ErrDocNewerContent):
					// Another build wrote an element this build does not know, during a release;
					// a build that reads it indexes the document, which stays owed until then.
					slog.Warn("dispatch: refs forms index skipped a document holding an element this build cannot read",
						"queue", queue.Table(), "artifact", source.ID, "error", err)
				case err != nil:
					slog.Error("dispatch: refs forms index skipped a document it could not index",
						"queue", queue.Table(), "artifact", source.ID, "error", err)
				case counted.counts(document):
					drained.count(&tick)
				}
			}
		default:
			slog.Error("dispatch: refs forms index skipped a row of a kind it does not drain",
				"queue", queue.Table(), "kind", source.Kind, "id", source.ID)
		}
		tick.Cursor = source
	}
	liveAhead.forgetUnlisted(after, taken, tick.Full)
	if !tick.Full {
		tick.Cursor = refs.Owed{}
	}
	if tick.OwedTexts, err = queue.Count(ctx, s.store.Pool); err != nil {
		return tick, err
	}
	return tick, nil
}

// formsLiveAhead is the forms drain's memo of the owed documents its passes found live ahead
// (indexOwedDocument), or whose issue room they found live ahead or holding a fragment that did not
// read back (indexOwedRoom), kept as the block index keeps its own (liveAheadDocuments) and for its
// reasons. documents holds each such document as it was listed then, which stillLiveAhead reads as
// the block index reads its memo: a document listed again with its version and its pending
// settlement's marked_at unchanged - for a document of an issue room, its room's - still holds the
// stored live text a pass found ahead, so it is counted live ahead, or unreadable, without being
// read, locked or owed a block index. rows is the queue row each was listed under, which says where
// in the queue's order forgetUnlisted looks for it.
type formsLiveAhead struct {
	documents liveAheadDocuments
	rows      map[string]refs.Owed
}

func newFormsLiveAhead() formsLiveAhead {
	return formsLiveAhead{documents: liveAheadDocuments{}, rows: map[string]refs.Owed{}}
}

// remember records document, listed under row, as found live ahead.
func (memo formsLiveAhead) remember(row refs.Owed, document owedDocument) {
	memo.documents[document.id] = document
	memo.rows[document.id] = row
}

// forget drops what memo holds of document artifactID.
func (memo formsLiveAhead) forget(artifactID string) {
	delete(memo.documents, artifactID)
	delete(memo.rows, artifactID)
}

// forgetUnlisted drops each document whose row sorts past after, up to the last of taken while the
// pass was full, that taken does not hold: its row is settled, or a write owed it again under a
// later enqueued_at, which moved its version or its settlement's mark too, so the walk reads it
// afresh where it now sorts.
func (memo formsLiveAhead) forgetUnlisted(after refs.Owed, taken []refs.Owed, full bool) {
	listed := make(map[string]struct{}, len(taken))
	for _, row := range taken {
		if row.Kind == "artifact" {
			listed[row.ID] = struct{}{}
		}
	}
	for id, row := range memo.rows {
		if _, found := listed[id]; found {
			continue
		}
		pastAfter := after.EnqueuedAt.IsZero() || after.Before(row)
		upToLast := !full || len(taken) > 0 && !taken[len(taken)-1].Before(row)
		if pastAfter && upToLast {
			memo.forget(id)
		}
	}
}

// formsDocumentDrain is what drainFormsDocument did with one document's row.
type formsDocumentDrain int

const (
	// formsDocumentIndexed: this drain indexed the document and settled its row.
	formsDocumentIndexed formsDocumentDrain = iota
	// formsDocumentGone: the document is gone or holds no markdown version, and its row was
	// settled.
	formsDocumentGone
	// formsDocumentLiveAhead: the document's live text is ahead of its latest version, and its
	// row is left owed.
	formsDocumentLiveAhead
	// formsDocumentUnreadable: a fragment of the document's issue room did not read back, and its
	// row is left owed.
	formsDocumentUnreadable
	// formsDocumentUnindexed: this drain's index did not run, and the row is left owed.
	formsDocumentUnindexed
)

// count adds what one document's drain did to tick.
func (drained formsDocumentDrain) count(tick *RefsFormsTick) {
	switch drained {
	case formsDocumentIndexed:
		tick.Documents++
	case formsDocumentGone:
		tick.Gone++
	case formsDocumentLiveAhead:
		tick.LiveAhead++
	case formsDocumentUnreadable:
		tick.Unreadable++
	case formsDocumentUnindexed:
		tick.Unindexed++
	}
}

// roomCounter is the issue rooms a pass over owed documents has counted, so it counts each room
// once, whichever of its documents the pass lists (ReconcileBlockIndex, the forms drain).
type roomCounter map[string]struct{}

// counts reports whether the pass counts document: a document that is its own room always, and a
// document of an issue room only where it is the first of its room the pass counts.
func (counted roomCounter) counts(document owedDocument) bool {
	if document.room == "" {
		return true
	}
	_, done := counted[document.room]
	counted[document.room] = struct{}{}
	return !done
}

// drainFormsDocument drains one `artifact` row of a forms queue, source, by indexing the
// document at its latest version (latestDocumentVersion) through the block index's own writer
// (indexOwedDocument, indexRenderedVersion).
//
// The index runs only while the row holds the enqueued_at it was listed with. A complete
// index settles the rows of every forms queue for the text it indexed. A row is not settled
// merely by owing a block index, as refs_grammar_owed's drain settles its own (drainOwedDocument):
// any build's block-index pass may take a document owed one, and a build that predates these forms would
// index it without them and leave nothing owed. So the document is owed a block index here
// (oweDocumentIndex), which indexOwedDocument requires of what it indexes, and indexed in the
// same pass. Whoever indexes it between the two leaves indexOwedDocument finding it no longer
// owed: an older build's block index leaves the row owed until this build's own index of the
// document commits, in a later pass, and a process of this build settles the row with its own
// index (refs.replaceRefs). A document whose live text is ahead of its latest version is left
// owed as the block index leaves it, for the settlement that writes its next version, and
// remembered in liveAhead as the block index remembers one; a version an older build writes then
// owes the row again (migration 20261117025904, and the relay of migration 20261126040100 once
// that build settles it), and one this build writes indexes the document and settles it.
//
// A document that is gone, holds no markdown version, is retired from its issue room, or is a
// Decisions query node (R15) owes no text. Without an index, this drain settles only source and
// only while it holds the enqueued_at it was listed with. A deletion from refs_forms_owed can
// relay a row while the document still exists (migration 20261126040100); the Decisions pass then
// settles that row the same way if it too finds no text to index.
//
// A document of an issue room is drained through its room's pass (drainFormsRoomDocument), since
// no pass indexes one fragment of a room alone. rooms holds what each room this drain's pass did.
func (s *Service) drainFormsDocument(ctx context.Context, source refs.Owed, document owedDocument, liveAhead formsLiveAhead, rooms map[string]owedOutcome) (formsDocumentDrain, error) {
	liveAhead.forget(document.id)
	if document.version == 0 {
		settled, err := refs.SettleOwed(ctx, s.store.Pool, source)
		if err != nil || !settled {
			return formsDocumentUnindexed, err
		}
		return formsDocumentGone, nil
	}
	if document.room != "" {
		return s.drainFormsRoomDocument(ctx, source, document, liveAhead, rooms)
	}
	if err := oweDocumentIndex(ctx, s.store.Pool, source.ID, s.markingRule); err != nil {
		return formsDocumentUnindexed, err
	}
	cold := s.srv.GetDoc(document.id) == nil
	indexed, ahead, err := s.indexOwedDocument(ctx, document, &source)
	switch {
	case err != nil:
		return formsDocumentUnindexed, err
	case ahead:
		if cold && !document.markedAt.IsZero() && s.srv.GetDoc(document.id) == nil {
			liveAhead.remember(source, document)
		}
		return formsDocumentLiveAhead, nil
	case indexed:
		return formsDocumentIndexed, nil
	}
	return formsDocumentUnindexed, nil
}

// drainFormsRoomDocument drains the `artifact` row source of one forms queue, for a document of
// the issue room document.room, through the block index's pass over the whole room (indexOwedRoom),
// at the room's revision (latestDocumentVersion). It reads every fragment with this build's
// grammar: the room's references under its tree artifact, each fragment's blocks, the mentions
// they hold and the excerpt each shows.
//
// The room is owed a block index first (oweDocumentIndex), of one artifact and no more: what its
// pass needs to win its fence. The pass's re-read lists the room owed by a mark of any of its
// artifacts, and its fence admits it where the row of a document it covers - a document of the
// room's layout holding a markdown version, the tree artifact included where it holds one - stands
// at no version. So the debt is document's own where the pass covers it, as it covers every other
// document latestDocumentVersion reads with its room, and for a tree artifact holding no markdown
// version, which the pass does not cover, the debt of a document the pass covers (roomPassDebt). A
// room whose pass covers no document has no text to index; the tree artifact's listed row alone
// is settled as a gone document's is. A complete pass settles forms rows only where the block
// pass won its fence: the tree artifact's with the room's references (replaceVersionRefs), and
// each other document it covered beside them (settleRoomDocumentsOwed). It has indexed all that
// text with every form under the owner lock, so those rows can be settled in all the forms queues.
//
// A room is indexed once a pass, whichever of its documents' rows the pass lists: rooms holds how
// each room the pass indexed fared, and a later row of the same room takes that outcome with no
// index or debt of its own. The outcomes are the block index's for a room (ReconcileBlockIndex): a
// room live ahead, or one a fragment of which did not read back, is left owed and its document
// remembered in liveAhead, keyed by the document, while no room of this server is resident for it.
func (s *Service) drainFormsRoomDocument(ctx context.Context, source refs.Owed, document owedDocument, liveAhead formsLiveAhead, rooms map[string]owedOutcome) (formsDocumentDrain, error) {
	cold := s.srv.GetDoc(document.resident()) == nil
	outcome, read := rooms[document.room]
	if !read {
		debt := document.id
		if document.id == document.room {
			var err error
			if debt, err = s.roomPassDebt(ctx, document.room); err != nil {
				return formsDocumentUnindexed, err
			}
			if debt == "" {
				settled, err := refs.SettleOwed(ctx, s.store.Pool, source)
				if err != nil || !settled {
					return formsDocumentUnindexed, err
				}
				return formsDocumentGone, nil
			}
		}
		if err := oweDocumentIndex(ctx, s.store.Pool, debt, s.markingRule); err != nil {
			return formsDocumentUnindexed, err
		}
		outcome = s.indexOwed(ctx, document, &source)
		rooms[document.room] = outcome
	}
	switch {
	case outcome.err != nil:
		return formsDocumentUnindexed, outcome.err
	case outcome.ahead:
		if cold && !document.markedAt.IsZero() && s.srv.GetDoc(document.resident()) == nil {
			liveAhead.remember(source, document)
		}
		return formsDocumentLiveAhead, nil
	case outcome.unreadable:
		if cold && s.srv.GetDoc(document.resident()) == nil {
			document.unreadable = true
			liveAhead.remember(source, document)
		}
		return formsDocumentUnreadable, nil
	case outcome.indexed:
		return formsDocumentIndexed, nil
	}
	return formsDocumentUnindexed, nil
}

// roomPassDebt is the artifact of the issue room room whose block index the forms drain owes so
// that the room's pass wins its fence (drainFormsRoomDocument): the tree artifact where it holds a
// markdown version, else the document of the room's layout (roomLayoutSQL) with the lowest id that
// holds one, and "" where no artifact the pass covers holds one.
func (s *Service) roomPassDebt(ctx context.Context, room string) (string, error) {
	var debt *string
	if err := s.store.Pool.QueryRow(ctx, `
		select coalesce(
			(select a.id from artifacts a where a.id = $1::uuid
			   and exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)),
			(select d.id from document_rooms held join artifacts d on d.id = held.artifact_id
			 where held.room = $1::uuid and d.retired_at is null
			   and exists (select 1 from artifact_versions v where v.artifact_id = d.id and v.markdown is not null)
			 order by d.id limit 1))::text
	`, room).Scan(&debt); err != nil {
		return "", fmt.Errorf("read the artifact room %s owes a block index: %w", room, err)
	}
	if debt == nil {
		return "", nil
	}
	return *debt, nil
}

// latestDocumentVersion is document artifactID as indexOwedDocument takes it and the block index
// lists it (owedDocuments): its latest markdown version, 0 for a document that holds none or is
// gone, which has no text to index, and its pending settlement's marked_at, zero when it owes
// none. indexOwedDocument reads that version again under the document's owner row, so a version
// written after this read leaves the document unindexed and its row owed.
//
// A document of an issue room - one its document_rooms row holds, or an issue's tree artifact -
// is read as owedDocuments reads one: its room, the room's revision (roomRevisionSQL) as its
// version and the room's pending settlement's marked_at, which indexOwedRoom reads again under the
// tree artifact's owner lock. A document of a room that no pass over the room reads, since it is
// retired from the room (roomLayoutSQL) or holds no markdown version, reads as no room's, at
// version 0: it has no text to index. So does an issue's decisions query node
// (refs.DecisionsQuerySQL): it holds no fragment of a room and no block (R15), and its stored
// rendering is never indexed. The tree artifact is read with its room whatever it holds, since the
// room's references are written under it.
func (s *Service) latestDocumentVersion(ctx context.Context, artifactID string) (owedDocument, error) {
	document := owedDocument{id: artifactID}
	var markedAt *time.Time
	if err := s.store.Pool.QueryRow(ctx, `
		select coalesce(placed.room::text, ''),
		       case when placed.room is not null then `+roomRevisionSQL("placed.room")+`
		            when placed.member is not null then 0
		            when exists (select 1 from artifacts a where a.id = $1::uuid and `+refs.DecisionsQuerySQL("a")+`) then 0
		            else coalesce((select max(v.number) from artifact_versions v
		                           where v.artifact_id = $1::uuid and v.markdown is not null), 0) end,
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = coalesce(placed.room, $1::uuid))
		from (
			select membership.room as member,
			       case when membership.room = $1::uuid or exists (
			                select 1 from artifacts a
			                where a.id = $1::uuid and a.retired_at is null
			                  and exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null))
			            then membership.room end as room
			from (select coalesce(
				(select moved.room from document_rooms moved where moved.artifact_id = $1::uuid),
				(select tree.artifact_id from issue_trees tree where tree.artifact_id = $1::uuid)) as room) membership
		) placed
	`, artifactID).Scan(&document.room, &document.version, &markedAt); err != nil {
		return owedDocument{}, fmt.Errorf("read the latest version of %s: %w", artifactID, err)
	}
	if markedAt != nil {
		document.markedAt = *markedAt
	}
	return document, nil
}
