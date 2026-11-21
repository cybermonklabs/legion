// comment_delete.go is DELETE /api/v1/comments/{id}: the author withdraws its own comment. The
// row and its place in the thread stay; the body goes, and with it every copy a read serves as
// the comment's own text that does not follow the cleared column — the block index, each
// block's stored excerpt, the text each revision recorded of a block, the document margin mark,
// the quote of an insert suggestion's anchor that no accept made document text, that insert's
// words in the quote of every other anchor on them, the name of the document version an
// accepted suggestion made, and the vector of each keyword row of a past version of the issue
// whose document version holds that insert's words (store.QueueTextVersionsMask) — each
// rewritten in the one transaction that clears it. The event log's payloads stay as written and
// are masked at each read (attachPostMarks), and so do the stored document versions that hold a
// pending insert's words, which every read of a version gives without them
// (docs.MaskedVersionText); a reply's events that quote the comment - its
// creation, and each later event of the reply but its own delete, written before this one - keep
// the preview they froze, as another anchor's events written before this delete keep the quote
// they froze. The delete's own comment.deleted, of a comment that is itself a reply, carries no
// text of the post it answers
// (commentCorrectionPayload). A pending suggestion's proof is in the document itself, so the same
// transaction rejects it before the clear, and where that reject takes an insert's words out of
// the document, writes the document's next version as its settlement would (docs.SettleVersion),
// so every store built from the latest version is written without the words before it commits.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deleteComment answers DELETE /api/v1/comments/{id}. Its prologue is the comment twin of the
// message delete's (lockedCommentForCorrection): the owner locked without the closed refusal,
// then the author and a post already deleted.
func (s *server) deleteComment(w http.ResponseWriter, r *http.Request) {
	if !requireUUIDPath(w, r, "comment") {
		return
	}
	var input actorInput
	if !s.decodeOptionalJSON(w, r, &input) {
		return
	}
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	comment, ok := s.lockedCommentForCorrection(w, r, tx, actor, "delete")
	if !ok {
		return
	}
	// The owner lock above decided this comment is deleted, a closed issue's included, so its
	// anchor's document writes are comment marks (docs.WithCommentMarks): the margin projection
	// below, which a closed issue takes as it takes the delete. A comment carrying a pending
	// suggestion takes the context here, where the create withholds it from one: the reject
	// below settles that mark and the clear takes the suggestion off the row, so the record the
	// projection writes names no pending suggestion, and a new one is what a closed issue
	// refuses.
	documentCtx = docs.WithCommentMarks(documentCtx)
	artifactName, err := s.commentArtifactName(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A pending suggestion's mark is proof in the document that the suggestion stands, and an
	// insert suggestion's text is in the document's own body (docs/reject.go, rejectedInsert).
	// The margin projection below rewrites the mark record and removes neither, and the clear
	// leaves an accept and a reject with nothing to move (409 POST_DELETED), so the delete
	// rejects the suggestion here, as the reject arm of commentAction does. It rejects it as the
	// comment's own anchor writes are made (docs.RejectDeletedCommentSuggestion), under the
	// owner lock this transaction holds: a closed issue still takes the delete of its comment,
	// so its document takes this reject too, where the accept and the reject routes are refused.
	// An orphaned mark is already out of the document: the delete records that on the anchor and
	// stands, where an accept and a reject answer 409.
	//
	// An insert suggestion's mark wraps the text it inserted, so its anchor's quote is that text
	// (resolveAnchor), the comment's own words; a replace's and a delete's wrap document text,
	// which stays in the document (docs.RejectDeletedCommentSuggestion). So the clear below
	// empties an insert's quote with its suggestion, and the anchor keeps its document, its block
	// and its mark id. Only an accept makes an insert's words document text: the quote of a
	// pending insert and of a rejected one is the comment's own, and is emptied, where an accepted
	// one keeps its quote and its version is renamed (renameAcceptedVersion, below).
	//
	// The kind is the mark's, recorded twice: the create stored it in the row's suggestion column
	// (storedSuggestion), which the read below takes by its key, and the reject answers it while
	// the mark stands. Both name the one mark's kind, so they agree; should a browser have
	// rewritten the mark's kind since, either one naming an insert empties the quote, since a
	// quote kept wrongly gives the comment's words where one emptied wrongly takes only document
	// text the document still holds. An orphaned mark answers no kind, and the margin record the
	// create wrote the kind into is rewritten without it by the first projection that finds the
	// mark gone (projectThreadMark), so the stored kind alone decides then: an insert's quote is
	// its words, a browser edit having changed some of them or not (an anchor refresh moves the
	// quote, never replace_with).
	//
	// A row made before the create stored the kind has none, and an orphaned suggestion of such
	// a row falls back to its own text: the words an insert put in the document are the words
	// its suggestion records, so the clear empties its quote when the quote is its replace_with,
	// byte for byte, and keeps any other, which no fact of that row tells from document text.
	//
	// A rejected suggestion's mark is gone already, and an insert's words with it: the reject
	// took them out of the document (commentAction, docs/reject.go rejectedInsert), so the delete
	// rejects nothing again. The reject keeps the kind the create stored (commentAction), which
	// alone decides, and a row with none falls back to its quote against its replace_with, as an
	// orphaned one does: markGone is that the suggestion's mark is out of the document, by a
	// reject or by a browser edit.
	//
	// An insert's words can carry other anchors' marks too - an ask's or another comment's, open
	// or closed, quoting words the insert added - and each such row's quote gives those words to
	// every read of it. The removal of the words names those marks (docs.TakenAnchorMarks): the
	// reject below answers them, and an earlier reject recorded them on the row
	// (storedSuggestion.Taken), which a pending suggestion's row holds none of. The delete has
	// every row on them written without the words (docs.RewriteTakenAnchors), here, before the
	// margin write below refreshes the document's anchors. A row rejected before the reject
	// recorded them holds none, and nothing stored tells which anchors that reject's removal took
	// text from, so the delete of such a row rewrites no other anchor. That holds only for a reject
	// made before this release, and no later reject can make such a row. The production Dispatch
	// held no reject at all when its whole event log was read on 2026-11-20 (dispatch://INFRA-1523,
	// census row 11.4).
	//
	// Everything up to Commit is the read phase (Docs.Join): this route and the document
	// operations queue their writes on the ledger, in the order made, and Commit sends them as
	// one batch.
	quoteIsBody := false
	// rejectedInsert is that the reject below took a pending insert suggestion's words out of the
	// document, which the delete then settles (docs.SettleVersion, below).
	rejectedInsert := false
	if comment.Suggestion != nil && comment.Anchor != nil && (comment.Suggestion.Accepted == nil || !*comment.Suggestion.Accepted) {
		storedKind, taken, err := loadStoredSuggestion(r.Context(), tx, comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		var kind string
		markGone := comment.Suggestion.Accepted != nil
		if !markGone {
			kind, taken, err = s.deps.Docs.RejectDeletedCommentSuggestion(documentCtx, comment.Anchor.ArtifactID, comment.Anchor.MarkID, actor)
			if err != nil {
				if !errors.Is(err, docs.ErrAnchorOrphaned) {
					s.writeHandlerError(w, err)
					return
				}
				if err := recordOrphanedCommentAnchor(ledger, &comment); err != nil {
					s.writeHandlerError(w, err)
					return
				}
				markGone = true
			}
		}
		rejectedInsert = kind == "insert"
		quoteIsBody = rejectedInsert || storedKind == "insert" ||
			(markGone && storedKind == "" && comment.Anchor.Quote == comment.Suggestion.ReplaceWith)
		if err := s.deps.Docs.RewriteTakenAnchors(documentCtx, comment.Anchor.ArtifactID, taken, actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// An accepted suggestion's proof is a version of the document, which the accept named
	// after the comment's body (acceptedVersionName); every read of the document's versions
	// serves that name, so the delete renames it here, beside the reject above, while the
	// comment still holds the anchor and the suggestion the clear below takes off it. The
	// comment.deleted below names the version with its new name (renamed_version).
	var renamedVersion *model.RenamedVersion
	if comment.Suggestion != nil && comment.Suggestion.Accepted != nil && *comment.Suggestion.Accepted && comment.Anchor != nil {
		renamed, err := renameAcceptedVersion(r.Context(), tx, ledger, comment)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		renamedVersion = renamed
	}
	// deleted_at is the transaction's now(), which the queued update writes, read here so the
	// projection, the event and the answer carry it before that update runs.
	deletedAt, err := ledger.Now(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	deletedAt = deletedAt.UTC()
	referenceChanges, clear, err := s.clearDeletedPostBody(
		ledger.ReadContext(r.Context()), tx, refs.ItemComment, comment.ID, comment.Body, quoteIsBody, actor, deletedAt)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Queue(clear); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A stored version that holds the words of an insert this comment suggested gives them to no
	// read once the delete commits (docs.MaskedVersionText), but the keyword table holds, for each
	// past version of the issue, the vector of its primary document version's stored markdown,
	// which a read cannot mask. The delete rewrites each such row's vector from the masked text in
	// its own write phase, after the clear, under the lock of the comment's owner it took before it
	// read the comment, which the keyword fill takes before it writes such rows, so a batch either
	// commits its rows before the rewrite reads them or reads the comment deleted
	// (store.QueueTextVersionsMask). An accepted insert's words are document text and stay.
	if comment.Anchor != nil {
		artifactID := comment.Anchor.ArtifactID
		numbers, texts, err := docs.MaskedVersionsOnDelete(r.Context(), tx, artifactID, comment.Anchor.MarkID, comment.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if len(numbers) > 0 {
			if err := ledger.Queue(func(b *store.WriteBatch) error {
				store.QueueTextVersionsMask(b, artifactID, numbers, texts)
				return nil
			}); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
	}
	comment.Body = ""
	comment.Suggestion = nil
	if quoteIsBody {
		comment.Anchor.Quote = ""
	}
	comment.DeletedAt = &deletedAt
	comment.DeletedBy = &actor
	if err := s.projectThreadMark(documentCtx, tx, ledger, comment, actor); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A pending insert's words were the post's own and never document text, and every store a
	// read builds from the document's latest version - the block index and the block texts, the
	// blocks' excerpts and the mentions' excerpts, search and the issue's search vector, the names
	// read and the related-item check - holds them until a version without them is written. The
	// settlement the reject arms writes that version a settle delay after the delete commits, so
	// the delete writes it here instead, as that settlement would (docs.SettleVersion): in its own
	// transaction, under the owner lock it holds, after every document write it makes, and every
	// one of those stores is written from the text without the words in the write phase the
	// delete commits with. It is the version that settlement would write, which then finds nothing
	// to version, so the delete adds no version and no approval move or carry the settlement would
	// not have made (SettleVersion says why). A reject of a replace or a delete takes no text out,
	// and a mark already gone takes nothing out either, so neither settles here.
	var settled *docs.VersionResult
	if rejectedInsert && !s.deleteLeavesSettlement {
		written, err := s.deps.Docs.SettleVersion(documentCtx, comment.Anchor.ArtifactID, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if written.Wrote {
			settled = &written
		}
	}
	// The answer is read here, in the read phase, with the attempts as they stand: the settle
	// below is queued, so the attempts it settles are set in the answer as it leaves them, and
	// each one's receipt is built from that. The settle refuses should a row no longer stand as
	// this read found it (queueDeletedPostSettlement).
	answer, err := s.hydrateWrittenComment(r.Context(), tx, comment)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var pending []model.CommentDelivery
	var receipts []model.Event
	for index := range answer.Deliveries {
		attempt := &answer.Deliveries[index]
		if attempt.State != model.DeliveryStatePending {
			continue
		}
		pending = append(pending, *attempt)
		attempt.State, attempt.Error = deliveryOutcome(deletedPostDeliveryError, false)
		receipts = append(receipts, commentDeliveryReceipt(comment, comment.Author, *attempt))
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		queueDeletedPostSettlement(b, comment.ID, commentAttemptsRead(pending))
		return nil
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := make([]*model.Event, 0, len(receipts)+1)
	for _, receipt := range receipts {
		appended, err := s.queueEvent(ledger, receipt)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, appended)
	}
	// Every fallback copy of the comment not yet settled (comment_fallback_copies, migration
	// 20261101180039) is settled with nothing published, and no fallback pass claims a settled
	// row: the text that copy carries is withdrawn, and the comment.deleted event below is what
	// each subscriber is told instead. A pass that claimed a copy before this delete committed is
	// a send in flight: it publishes the copy as recorded, though the row reads published = false.
	// Nor does this reach the outbox's own copy of the comment.created event, held while a mention
	// of the comment sends: that copy is the event as Dispatch recorded it, and it goes as
	// recorded.
	if err := queueExec(ledger, `
		update comment_fallback_copies set settled_at = now(), published = false
		where comment_id = $1 and settled_at is null
	`, comment.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload, logins, err := s.commentCorrectionPayload(r.Context(), tx, &comment, artifactName, referenceChanges)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	payload.RenamedVersion = renamedVersion
	event, err := s.queueLoggedEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID).event(
		"comment.deleted",
		actor,
		payload,
	), logins, "comment", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	// The settled version's artifact.version follows the delete's own event, as the settlement's
	// would have, and carries no diff, as the settlement's carries none.
	if settled != nil {
		versionEvent, err := s.commitArtifactVersionEvent(ledger, ownerOf(comment.IssueKey, comment.ArtifactID), actor, comment.Anchor.ArtifactID, artifactName, *settled, nil)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, versionEvent)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), appendedEvents(events...)...)
	WriteJSON(w, http.StatusOK, answer)
}

// renameAcceptedVersion renames the document version comment's accepted suggestion made to the
// name an accept gives a suggestion with no body (acceptedVersionName), in the delete's own
// transaction, so no read of the document's versions serves any part of the deleted body as a
// version's name, and marks it renamed by this comment's delete in the same update
// (artifact_versions.renamed_by_delete, which answerEndedByDelete reads). It answers the version
// it renamed with the name it gave it, nil when it renamed none. The artifact.version
// event the accept appended keeps the name it carried: no delete rewrites a stored event, and
// only the deleted comment's own events are read without its body (attachPostMarks). The version
// is found in the delete's read phase, under the comment's owner lock, and its update is queued
// on ledger.
//
// No event of the document says the rename: an artifact.renamed corrects the document's title,
// and every reader prints it as one ("renamed X to X"), and an artifact.version announces a
// version written, whose readers count it as new work. So the delete's own comment.deleted names
// the version and its new name (model.CommentEventPayload.RenamedVersion), and a reader holding
// the document's versions takes the name from it.
//
// The accept records the version it made by the comment's id (artifact_versions.accepted_from,
// queued in commentAction's transaction right after Docs.NamedVersion), so the version is the one
// of the anchor's document that link names, whatever other version carries its time. A suggestion
// accepted before the accept recorded that link has no version naming it, and only for such a
// comment is the version found by the time the accept wrote it. The accept writes the version
// (Docs.NamedVersion), the version's artifact.version event and the comment's suggestion.accepted
// event in one transaction, and the version's created_at and each event's are that transaction's
// now(), the time it began; nothing updates either afterwards. (The comment's own resolved_at is
// no such mark: a later resolve moves it and a reopen clears it.) The version is therefore the
// named version of the anchor's document whose created_at is the time of
// the comment's suggestion.accepted event, read within the comment's owner, as
// loadCommentCreatedEvent reads its comment.created event. Another named version of that document
// carries the same time only when a transaction that began in the same microsecond wrote it, and
// the accept's own transaction writes no other: a lookup that finds more than one version renames
// none, and one that finds none - an accept whose event or version is not where this reads them -
// renames none either, each logged with the comment and the document.
func renameAcceptedVersion(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, comment model.Comment) (*model.RenamedVersion, error) {
	artifactID := comment.Anchor.ArtifactID
	rows, err := tx.Query(ctx, `
		select number from artifact_versions where artifact_id = $1 and accepted_from = $2
	`, artifactID, comment.ID)
	if err != nil {
		return nil, err
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, err
	}
	if len(numbers) == 0 {
		ownerColumn, ownerID := commentEventOwner(comment)
		rows, err = tx.Query(ctx, `
			select v.number from artifact_versions v
			where v.artifact_id = $3 and v.named and v.created_at in (
				select e.created_at from events e
				where e.`+ownerColumn+` = $1 and e.type = 'suggestion.accepted' and e.payload ->> 'id' = $2
			)
		`, ownerID, comment.ID, artifactID)
		if err != nil {
			return nil, err
		}
		if numbers, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return nil, err
		}
	}
	if len(numbers) != 1 {
		slog.Warn("dispatch: a deleted comment's accepted suggestion names no single document version, so no version is renamed",
			"comment", comment.ID, "artifact", artifactID, "versions", numbers)
		return nil, nil
	}
	renamed := model.RenamedVersion{
		ArtifactID: artifactID,
		Number:     numbers[0],
		Summary:    acceptedVersionName("", comment.Anchor.Quote, comment.Suggestion.ReplaceWith),
	}
	return &renamed, queueExec(ledger, `
		update artifact_versions set summary = $3, renamed_by_delete = $4 where artifact_id = $1 and number = $2
	`, artifactID, renamed.Number, renamed.Summary, comment.ID)
}
