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
	// so and that are still as it found them included, and Unindexed how many it left owed
	// because its own index did not run: another process indexed the document first, or a write
	// moved the document's latest version or the row's enqueued_at meanwhile.
	LiveAhead int
	Unindexed int
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
	seed := newFormsDrain(refs.RefsDecisionsFormsSeedOwed)
	seed.oneShot = true
	return []*formsDrain{newFormsDrain(refs.RefsFormsOwed), newFormsDrain(refs.RefsDecisionsFormsOwed), seed}
}

// refsFormsTick runs one pass of each drain that is not done, in order, and returns how long to
// wait before the next tick: blockIndexStep when any pass reported a full batch,
// BlockIndexReconcileInterval otherwise. A pass that fails or is full never keeps the passes after
// it from running; only the end of ctx does.
func (s *Service) refsFormsTick(ctx context.Context, drains []*formsDrain) time.Duration {
	wait := BlockIndexReconcileInterval
	for _, drain := range drains {
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
		tick.Unindexed != drain.logged.Unindexed || tick.OwedTexts != drain.logged.OwedTexts {
		slog.Info("dispatch: refs forms index", "queue", drain.queue.Table(), "texts", tick.Texts,
			"documents", tick.Documents, "gone", tick.Gone, "live_ahead", tick.LiveAhead,
			"unindexed", tick.Unindexed, "owed_texts", tick.OwedTexts)
		drain.logged = tick
	}
	drain.cursor = tick.Cursor
	if drain.oneShot && err == nil && tick.OwedTexts == 0 && !tick.Full {
		drain.done = true
		slog.Info("dispatch: refs forms index found its bootstrap queue empty, which only its migration writes, and reads it no more",
			"queue", drain.queue.Table())
	}
	return tick.Full
}

// ReconcileRefsForms runs one pass over queue, refs.RefsFormsOwed or refs.RefsDecisionsFormsOwed:
// it lists at most blockIndexItemBatch rows of it past after, in the queue's order, oldest first
// (refs.ListOwedAfter), and drains each in a transaction of its own. A comment, an ask or an
// issue message goes through refs.ReindexOwed, which re-indexes its text with this build's
// grammar and settles its row of the queue in the same transaction, and with it the row of the
// other queue. A document is indexed here, at its latest version, by the block index's own writer
// (drainFormsDocument), at most blockIndexDocumentBatch of them a pass, the bound the block index
// reads documents under; the pass ends at the first document past that bound, and the next pass,
// which comes after blockIndexStep, starts there.
//
// The rows are listed by keyset, as the block index lists its documents, so a row this drain
// leaves owed never holds back the rows past it: a document whose live text is ahead, one holding
// an element this build does not know (ErrDocNewerContent), a row that fails for a reason of its
// own, each logged and left owed. The pass's Cursor moves past every row it takes, drained or
// not, and goes back to the start once a listing is not full, so the next walk of the queue
// reads each such row again, once. liveAhead is the documents the calling loop's earlier passes
// over this queue found live ahead, which this pass counts again without reading or owing them
// an index while their version and their settlement's mark stand as found, and keeps up to date.
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
			switch {
			case ctx.Err() != nil:
				return tick, ctx.Err()
			case err != nil:
				slog.Error("dispatch: refs forms index skipped a document it could not index",
					"queue", queue.Table(), "artifact", source.ID, "error", err)
			case s.stillLiveAhead(liveAhead.documents, document):
				tick.LiveAhead++
				liveAhead.rows[document.id] = source
			case documents == blockIndexDocumentBatch:
				// This document and the rows after it wait for the next pass, which starts here.
				taken, tick.Full = owed[:index], true
				break rows
			default:
				documents++
				drained, err := s.drainFormsDocument(ctx, source, document, liveAhead)
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
				default:
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
// (indexOwedDocument), kept as the block index keeps its own (liveAheadDocuments) and for its
// reasons. documents holds each such document as it was listed then, which stillLiveAhead reads as
// the block index reads its memo: a document listed again with its version and its pending
// settlement's marked_at unchanged still holds the stored live text a pass found ahead, so it is
// counted live ahead without being read, locked or owed a block index. rows is the queue row each
// was listed under, which says where in the queue's order forgetUnlisted looks for it.
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
	case formsDocumentUnindexed:
		tick.Unindexed++
	}
}

// drainFormsDocument drains one `artifact` row of a forms queue (refs_forms_owed or
// refs_decisions_forms_owed), source, by indexing the document itself, at document's latest
// version (latestDocumentVersion), through the index the block index writes a document with
// (indexOwedDocument, indexRenderedVersion): its own mentions, the mentions its blocks hold and
// the excerpt each shows, read with this build's grammar.
//
// The row is settled by this drain only through its own index of the document, in that index's
// transaction (indexOwedDocument's settle, which clears the document's row of both queues), and
// only while it holds the enqueued_at it was listed with. It is not settled by owing the document
// a block index, as refs_grammar_owed's drain settles its own (drainOwedDocument): any build's
// block-index pass may take a document owed one, and a build that predates these forms would
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
// A document that is gone, or holds no markdown version, owes no text and is owed no block index:
// its row is settled while it holds the enqueued_at it was listed with. An older build's removal
// of a document deletes its edges, which owes the first queue the document (migration
// 20261117025906), and its settle of that queue relays the row to the second only while the
// document's own row stands (migration 20261126040100), so a removal leaves at worst a row this
// settles as gone.
func (s *Service) drainFormsDocument(ctx context.Context, source refs.Owed, document owedDocument, liveAhead formsLiveAhead) (formsDocumentDrain, error) {
	liveAhead.forget(document.id)
	if document.version == 0 {
		settled, err := refs.SettleOwed(ctx, s.store.Pool, source)
		if err != nil || !settled {
			return formsDocumentUnindexed, err
		}
		return formsDocumentGone, nil
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

// latestDocumentVersion is document artifactID as indexOwedDocument takes it and the block index
// lists it (owedDocuments): its latest markdown version, 0 for a document that holds none or is
// gone, which has no text to index, and its pending settlement's marked_at, zero when it owes
// none. indexOwedDocument reads that version again under the document's owner row, so a version
// written after this read leaves the document unindexed and its row owed.
func (s *Service) latestDocumentVersion(ctx context.Context, artifactID string) (owedDocument, error) {
	document := owedDocument{id: artifactID}
	var markedAt *time.Time
	if err := s.store.Pool.QueryRow(ctx, `
		select coalesce((select max(v.number) from artifact_versions v
		                 where v.artifact_id = $1::uuid and v.markdown is not null), 0),
		       (select p.marked_at from doc_settlements_pending p where p.artifact_id = $1::uuid)
	`, artifactID).Scan(&document.version, &markedAt); err != nil {
		return owedDocument{}, fmt.Errorf("read the latest version of %s: %w", artifactID, err)
	}
	if markedAt != nil {
		document.markedAt = *markedAt
	}
	return document, nil
}
