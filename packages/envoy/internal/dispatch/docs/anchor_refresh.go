package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type anchoredMark struct {
	id       string
	markType string
	anchor   model.Anchor
	// pending is the row's registration on the transaction's ledger (RegisterPendingAnchor), nil
	// for a row read as the table stores it.
	pending *PendingAnchor
}

// markRef is the mark identity the row anchors to.
func (m anchoredMark) markRef() pmdoc.MarkRef {
	return pmdoc.MarkRef{Type: m.markType, ID: m.anchor.MarkID}
}

// openAnchoredMarksQuery is the statement openAnchoredMarks runs over target's table: every row
// anchored on the document bound to $1 whose mark is still open, oldest first. The open filter
// is the target's own (anchorTarget.open), so a table added to anchorTargets brings the
// predicate that holds its rows open with it. It takes no row lock: the caller holds the
// document owner's lock, which every writer of an anchored row takes first.
func openAnchoredMarksQuery(target anchorTarget) string {
	return fmt.Sprintf(`
			select id::text, anchor, %s
			from %s
			where %s and %s
			order by created_at, id
		`, commentMarkType(target.table), target.table, target.anchoredOn(), target.open)
}

// openAnchoredMarks lists artifactID's open anchors - the open anchored asks, then the unresolved
// anchored comments, each by creation time and id, so a refresh announces its rows, and an upload
// re-anchors them, in the same order every time it runs over the same rows - as the transaction
// sees them: the rows source holds, with the anchored rows the transaction has queued writes of
// and registered (RegisterPendingAnchor) merged in as they will stand. A stored row so registered
// is read as registered, in its place, or left out once closed; a registered row the table does
// not list follows the stored rows of its table, in the order it was registered.
func (s *Service) openAnchoredMarks(ctx context.Context, source Queryer, artifactID string) ([]anchoredMark, error) {
	pending := pendingAnchorsOf(ctx, artifactID)
	var marks []anchoredMark
	for _, target := range anchorTargets {
		rows, err := source.Query(ctx, openAnchoredMarksQuery(target), artifactID)
		if err != nil {
			return nil, fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		listed := make(map[string]struct{})
		for rows.Next() {
			var mark anchoredMark
			var encoded []byte
			var typeFromRow string
			if err := rows.Scan(&mark.id, &encoded, &typeFromRow); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s anchor: %w", target.table, err)
			}
			if err := model.DecodeAnchor(encoded, &mark.anchor); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode %s anchor: %w", target.table, err)
			}
			mark.markType = typeFromRow
			listed[mark.id] = struct{}{}
			if row := pendingAnchor(pending, mark.id); row != nil {
				if !row.Open {
					continue
				}
				mark = anchoredMark{id: row.ID, markType: string(row.Kind), anchor: row.Anchor, pending: row}
			}
			marks = append(marks, mark)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		rows.Close()
		for _, row := range pending {
			if _, stored := listed[row.ID]; stored || !row.Open || anchorTable(string(row.Kind)) != target.table {
				continue
			}
			marks = append(marks, anchoredMark{id: row.ID, markType: string(row.Kind), anchor: row.Anchor, pending: row})
		}
	}
	return marks, nil
}

// pendingAnchor is the registration of row id among pending, nil when it has none.
func pendingAnchor(pending []*PendingAnchor, id string) *PendingAnchor {
	for _, row := range pending {
		if row.ID == id {
			return row
		}
	}
	return nil
}

// anchorTable is the table whose rows carry a mark of markType.
func anchorTable(markType string) string {
	if markType == string(MarkComment) || markType == string(MarkSuggestion) {
		return "comments"
	}
	return "asks"
}

func commentMarkType(table string) string {
	if table == "comments" {
		return `case when suggestion is null then 'proofComment' else 'proofSuggestion' end`
	}
	return `'dispatchAsk'`
}

// changedAnchor is an anchor whose quote or orphaned flag a write changes: an open anchor its
// current tree mark moves (refreshAnchors), or a row on a mark the reject of an insert took text
// from (RewriteTakenAnchors). comment is whether its row is a comment's (a comment or a
// suggestion) rather than an ask's, closed whether its row is closed (a resolved comment, an ask
// no longer open), which no refresh reads, and event builds the row's refresh event once the
// write phase reaches it.
type changedAnchor struct {
	mark      anchoredMark
	refreshed model.Anchor
	comment   bool
	closed    bool
	event     func() (model.Event, error)
}

// refreshedAnchors is the changed anchors of one table as the columns its update writes, one
// entry per anchor, in the order found.
type refreshedAnchors struct {
	table    string
	ids      []string
	quotes   []string
	orphaned []bool
}

// removedAnchorSkipKey keys the *removedAnchorSkip of a deleted comment's reject
// (RejectDeletedCommentSuggestion).
type removedAnchorSkipKey struct{}

// removedAnchorSkip is the anchor marks a deleted comment's reject took out whole with an insert's
// words (TakenAnchorMarks.Removed). applySuggestion records them as it makes the removal, and its
// operation then records the write and refreshes the document's anchors against the tree it left,
// under the same context (recordLiveOperation). That refresh leaves each row on one of these marks
// as it stands (refreshAnchors): it would orphan the row keeping its quote, the deleted post's
// words, and announce that, where the delete writes every row on such a mark, of any state, with
// an empty quote (RewriteTakenAnchors).
type removedAnchorSkip struct{ marks map[pmdoc.MarkRef]bool }

// refreshAnchors updates each open anchor from its current tree mark. Each persisted change
// appends the affected row's own full refresh event in this transaction; callers publish the
// collected committed events afterward.
//
// Every changed anchor is found, and every event it owes read, before anything is written, and
// then all of them are written together: one update of each table and one append per event, put
// on the transaction's write phase as one step (write), so a transaction whose writes are queued
// runs them in its write phase and one whose writes are not runs them as a batch of its own. The
// round trips spent holding the document's owner, which every event writer of that owner waits
// for, therefore stay the same however many anchors changed.
//
// The rows the events carry are read before their updates run, so each is given the quote and
// orphaned flag its update writes, which is the row the update leaves. A row this transaction has
// only queued the write of is not in the table at all: it comes from the ledger
// (openAnchoredMarks, PendingAnchor), and its event is built from what the registration prepared
// rather than read back. Where the step is queued, the ledger records each refreshed anchor
// (RegisterPendingAnchor), so a later refresh in the same transaction reads the anchor this one
// leaves.
//
// A deleted comment's reject also leaves out every row on a mark it took out whole with the
// insert's words (removedAnchorSkip): the delete writes those rows itself (RewriteTakenAnchors).
func (s *Service) refreshAnchors(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, actor model.Actor) error {
	anchors, err := s.openAnchoredMarks(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	skippedMarkID, skipping := ctx.Value(skippedAnchorRefreshKey{}).(string)
	removedSkip, _ := ctx.Value(removedAnchorSkipKey{}).(*removedAnchorSkip)
	skipped := func(mark anchoredMark) bool {
		return (skipping && mark.anchor.MarkID == skippedMarkID) || (removedSkip != nil && removedSkip.marks[mark.markRef()])
	}
	// One walk finds every anchor: a walk per row would make each write quadratic in a document
	// carrying many anchors. Rows sharing a key look the same result up, each refreshed in turn.
	keys := make([]pmdoc.MarkRef, 0, len(anchors))
	for _, mark := range anchors {
		if !skipped(mark) {
			keys = append(keys, mark.markRef())
		}
	}
	found := pmdoc.FindMarks(tree, keys)
	var changed []changedAnchor
	for _, mark := range anchors {
		if skipped(mark) {
			continue
		}
		refreshed := mark.anchor
		if hit, ok := found[mark.markRef()]; ok {
			refreshed.Quote = hit.Quote
			refreshed.Orphaned = false
		} else {
			refreshed.Orphaned = true
		}
		if refreshed == mark.anchor {
			continue
		}
		comment := anchorTable(mark.markType) == "comments"
		changed = append(changed, changedAnchor{mark: mark, refreshed: refreshed, comment: comment})
	}
	if s.afterAnchorRefresh != nil {
		s.afterAnchorRefresh(artifactID, len(changed))
	}
	if len(changed) == 0 {
		return nil
	}
	return s.writeChangedAnchors(ctx, tx, changed, actor)
}

// writeChangedAnchors writes changed, the anchors a write moves, as refreshAnchors says: it reads
// every event they owe, registers each one on the transaction's ledger as it will stand
// (RegisterPendingAnchor), open or closed as its row is, and puts one update of each table and
// one append per event on the write phase as one step. refreshAnchors and RewriteTakenAnchors both
// write through it, so a row either moves is written, announced and registered alike.
func (s *Service) writeChangedAnchors(ctx context.Context, tx pgx.Tx, changed []changedAnchor, actor model.Actor) error {
	changedAsks, changedComments := refreshedAnchors{table: "asks"}, refreshedAnchors{table: "comments"}
	// A row the transaction has only queued carries its own event builder, so it is left out of
	// the reads by id: the table holds no row of it to read.
	var storedAskIDs, storedCommentIDs []string
	for _, anchor := range changed {
		into := &changedAsks
		stored := &storedAskIDs
		if anchor.comment {
			into, stored = &changedComments, &storedCommentIDs
		}
		into.ids = append(into.ids, anchor.mark.id)
		into.quotes = append(into.quotes, anchor.refreshed.Quote)
		into.orphaned = append(into.orphaned, anchor.refreshed.Orphaned)
		if anchor.mark.pending == nil || anchor.mark.pending.Event == nil {
			*stored = append(*stored, anchor.mark.id)
		}
	}
	asks, err := loadAnchorRefreshedAsks(ctx, tx, storedAskIDs)
	if err != nil {
		return err
	}
	comments, err := loadAnchorRefreshedCommentPayloads(ctx, tx, storedCommentIDs)
	if err != nil {
		return err
	}
	ledger := queueFor(ctx, tx)
	for index := range changed {
		anchor := &changed[index]
		if anchor.event, err = s.anchorRefreshEvent(ctx, tx, *anchor, asks, comments, actor); err != nil {
			return err
		}
		if ledger != nil {
			row := PendingAnchor{
				ID: anchor.mark.id, Kind: MarkKind(anchor.mark.markType), Anchor: anchor.refreshed, Open: !anchor.closed,
			}
			if anchor.mark.pending != nil {
				row.Event = anchor.mark.pending.Event
			}
			ledger.RegisterPendingAnchor(row)
		}
	}
	sink := eventSink(ctx)
	return write(ctx, tx, func(b *store.WriteBatch) error {
		for _, update := range []refreshedAnchors{changedAsks, changedComments} {
			if len(update.ids) == 0 {
				continue
			}
			// Only the two fields this refresh owns are written. The rows were read before the
			// events and lookups, so writing the whole anchor column back would erase what
			// another writer - the one-time block-id backfill - put in it in between (LEGION-149).
			b.Queue("update "+update.table+" anchor", fmt.Sprintf(`
				update %s as target
				set anchor = jsonb_set(
					jsonb_set(target.anchor, '{quote}', to_jsonb(refreshed.quote)),
					'{orphaned}', to_jsonb(refreshed.orphaned))
				from unnest($1::uuid[], $2::text[], $3::boolean[]) as refreshed(id, quote, orphaned)
				where target.id = refreshed.id
			`, update.table), update.ids, update.quotes, update.orphaned)
		}
		for _, anchor := range changed {
			event, err := anchor.event()
			if err != nil {
				return err
			}
			if err := s.events.QueueAppend(b, event, sink); err != nil {
				return fmt.Errorf("append anchor refresh event: %w", err)
			}
		}
		return nil
	})
}

// takenAnchorRowsQuery is the statement RewriteTakenAnchors runs over target's table: every row
// anchored on the document bound to $1 whose anchor names one of the mark ids bound to $2, open or
// closed, oldest first, with the type of the mark it anchors and whether target holds it open.
func takenAnchorRowsQuery(target anchorTarget) string {
	return fmt.Sprintf(`
			select id::text, anchor, %s, (%s)
			from %s
			where %s and anchor->>'mark_id' = any($2::text[])
			order by created_at, id
		`, commentMarkType(target.table), target.open, target.table, target.anchoredOn())
}

// RewriteTakenAnchors writes the anchor of every ask and comment row on artifactID, open or
// closed, anchored on a mark the reject of an insert suggestion took text from (TakenAnchorMarks),
// so that no read of the row gives a word the insert put in the document: an insert's words that
// no accept made document text were only ever its comment's, and that comment's delete withdraws
// them. It writes in the caller's joined transaction, under the document owner's lock the caller
// holds, as an anchor refresh writes (writeChangedAnchors), and each row it changes is announced
// in its comment.anchor_refreshed or ask.anchor_refreshed as a refresh's is. An anchor is a field
// the document derives, which every refresh rewrites as the document changes, so this edits no
// post.
//
//   - A row on a mark the removal took out whole (taken.Removed) quoted those words alone: its quote
//     is emptied, and it stands orphaned, whatever its state.
//   - An open row on a mark the removal cut (taken.Cut) is the anchor refresh's: the one the removal
//     ran gave it the mark's text in the tree the removal left, as every later refresh does. A
//     closed row on such a mark, which no refresh reads, is given its mark's text in the document
//     as the transaction holds it now, as a refresh computes a quote (pmdoc.FindMarks), and stands;
//     where that document no longer holds the mark, no text of it is left to give, and its quote is
//     emptied with it orphaned, since the quote it holds may be the inserted words.
//
// A row is read as the transaction will leave it, its registration on the ledger in place of the
// stored row (RegisterPendingAnchor), and one already standing as it would be written is left
// alone, so a row this transaction already wrote is not announced twice.
func (s *Service) RewriteTakenAnchors(ctx context.Context, artifactID string, taken TakenAnchorMarks, actor model.Actor) error {
	if len(taken.Removed) == 0 && len(taken.Cut) == 0 {
		return nil
	}
	tx, ok := txFromContext(ctx)
	if !ok {
		return errUnjoined
	}
	removed, cut := markSet(taken.Removed), markSet(taken.Cut)
	pending := pendingAnchorsOf(ctx, artifactID)
	type takenRow struct {
		mark anchoredMark
		open bool
	}
	var rows []takenRow
	for _, target := range anchorTargets {
		// Only the ids of the marks this table's rows carry (anchorTable): a table none of them
		// belongs to is not read.
		var markIDs []string
		for _, marks := range [][]AnchorMark{taken.Removed, taken.Cut} {
			for _, mark := range marks {
				if anchorTable(mark.Type) == target.table {
					markIDs = append(markIDs, mark.ID)
				}
			}
		}
		if len(markIDs) == 0 {
			continue
		}
		listed, err := tx.Query(ctx, takenAnchorRowsQuery(target), artifactID, markIDs)
		if err != nil {
			return fmt.Errorf("list %s anchors on taken marks: %w", target.table, err)
		}
		for listed.Next() {
			var row takenRow
			var encoded []byte
			if err := listed.Scan(&row.mark.id, &encoded, &row.mark.markType, &row.open); err != nil {
				listed.Close()
				return fmt.Errorf("scan %s anchor on a taken mark: %w", target.table, err)
			}
			if err := model.DecodeAnchor(encoded, &row.mark.anchor); err != nil {
				listed.Close()
				return fmt.Errorf("decode %s anchor on a taken mark: %w", target.table, err)
			}
			if registered := pendingAnchor(pending, row.mark.id); registered != nil {
				row.mark = anchoredMark{id: registered.ID, markType: string(registered.Kind), anchor: registered.Anchor, pending: registered}
				row.open = registered.Open
			}
			if ref := row.mark.markRef(); removed[ref] || cut[ref] {
				rows = append(rows, row)
			}
		}
		listed.Close()
		if err := listed.Err(); err != nil {
			return fmt.Errorf("list %s anchors on taken marks: %w", target.table, err)
		}
	}
	// The document is read once, and only for a closed row on a cut mark.
	var found map[pmdoc.MarkRef]pmdoc.MarkFound
	var changed []changedAnchor
	for _, row := range rows {
		ref := row.mark.markRef()
		refreshed := row.mark.anchor
		switch {
		case removed[ref]:
			refreshed.Quote, refreshed.Orphaned = "", true
		case row.open:
			continue
		default:
			if found == nil {
				tree, err := s.docTree(commentMarkOperation(ctx), artifactID)
				if err != nil {
					return err
				}
				keys := make([]pmdoc.MarkRef, 0, len(taken.Cut))
				for _, mark := range taken.Cut {
					keys = append(keys, pmdoc.MarkRef(mark))
				}
				found = pmdoc.FindMarks(tree, keys)
			}
			// A mark the document no longer holds leaves no text to give: hit is then zero.
			hit, stands := found[ref]
			refreshed.Quote, refreshed.Orphaned = hit.Quote, !stands
		}
		if refreshed == row.mark.anchor {
			continue
		}
		changed = append(changed, changedAnchor{
			mark: row.mark, refreshed: refreshed, comment: anchorTable(row.mark.markType) == "comments", closed: !row.open,
		})
	}
	if len(changed) == 0 {
		return nil
	}
	return s.writeChangedAnchors(ctx, tx, changed, actor)
}

// anchorRefreshEvent builds the anchor-refresh event of anchor's row as its update leaves it: the
// row with the quote and orphaned flag that update writes. A row the transaction registered with
// an event of its own (PendingAnchor.Event) prepares it now, in the read phase, and builds it when
// the write phase reaches it; any other is taken from the rows read by id here, where the refresh
// is worked out, and given the two fields the update writes.
func (s *Service) anchorRefreshEvent(
	ctx context.Context,
	tx pgx.Tx,
	anchor changedAnchor,
	asks map[string]model.Ask,
	comments map[string]model.CommentEventPayload,
	actor model.Actor,
) (func() (model.Event, error), error) {
	if anchor.mark.pending != nil && anchor.mark.pending.Event != nil {
		build, err := anchor.mark.pending.Event(ctx, tx)
		if err != nil {
			return nil, err
		}
		refreshed := anchor.refreshed
		return func() (model.Event, error) { return build(refreshed) }, nil
	}
	var event model.Event
	if !anchor.comment {
		ask := asks[anchor.mark.id]
		if ask.Anchor != nil {
			ask.Anchor.Quote, ask.Anchor.Orphaned = anchor.refreshed.Quote, anchor.refreshed.Orphaned
		}
		// Re-anchoring moves the mark, never the question text, so it cites nothing new.
		event = model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type:    "ask.anchor_refreshed",
			Actor:   actor,
			Payload: model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		}
	} else {
		payload := comments[anchor.mark.id]
		if payload.Comment.Anchor != nil {
			payload.Comment.Anchor.Quote, payload.Comment.Anchor.Orphaned = anchor.refreshed.Quote, anchor.refreshed.Orphaned
		}
		event = model.Event{
			IssueKey: payload.IssueKey, ArtifactID: payload.ArtifactID,
			Type: "comment.anchor_refreshed", Actor: actor, Payload: payload,
		}
	}
	return func() (model.Event, error) { return event, nil }, nil
}

// loadAnchorRefreshedAsks reads each ask of ids as its refresh event carries it, by id, in one
// read of the rows and one of their opened events. Every id must name an ask.
func loadAnchorRefreshedAsks(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.Ask, error) {
	asks := make(map[string]model.Ask, len(ids))
	if len(ids) == 0 {
		return asks, nil
	}
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`, ba.id::text, ba.slug, ba.is_primary,
			aa.project_key, aa.slug, aa.name, aa.is_primary
		from asks a
		left join artifacts ba on ba.id = a.block_artifact_id
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
		where a.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load refreshed asks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blockArtifactID, blockArtifactSlug *string
		var blockArtifactPrimary *bool
		var anchorProject, anchorSlug, anchorName *string
		var anchorPrimary *bool
		ask, err := ScanAsk(rows, &blockArtifactID, &blockArtifactSlug, &blockArtifactPrimary, &anchorProject, &anchorSlug, &anchorName, &anchorPrimary)
		if err != nil {
			return nil, fmt.Errorf("load refreshed asks: %w", err)
		}
		if blockArtifactID != nil {
			ask.BlockArtifact = &model.AskBlockArtifact{
				ID: *blockArtifactID, Slug: *blockArtifactSlug, Primary: *blockArtifactPrimary,
			}
		}
		if anchorProject != nil {
			ask.AnchorArtifact = &model.AskAnchorArtifact{
				Project: *anchorProject, Slug: *anchorSlug, Name: *anchorName, Primary: *anchorPrimary,
			}
		}
		asks[ask.ID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load refreshed asks: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if _, found := asks[id]; !found {
			return nil, fmt.Errorf("load refreshed ask %q: %w", id, pgx.ErrNoRows)
		}
	}
	openedEventIDs, err := events.OpenedEventIDs(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("load refreshed asks' opened events: %w", err)
	}
	for _, id := range ids {
		openedEventID, ok := openedEventIDs[id]
		if !ok {
			return nil, fmt.Errorf("load refreshed ask %q opened event: ask has no event", id)
		}
		ask := asks[id]
		ask.OpenedEventID = &openedEventID
		asks[id] = ask
	}
	return asks, nil
}

// loadAnchorRefreshedCommentPayloads reads each comment of ids as its refresh event carries it,
// with the name and place of the artifact its anchor is on, by id: one read each of the rows with
// their anchors' artifacts, their mentions and their deliveries. Every id must name an anchored
// comment.
func loadAnchorRefreshedCommentPayloads(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.CommentEventPayload, error) {
	payloads := make(map[string]model.CommentEventPayload, len(ids))
	if len(ids) == 0 {
		return payloads, nil
	}
	comments, places, err := loadAnchorRefreshedComments(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if comments[id].Anchor == nil {
			return nil, fmt.Errorf("refreshed comment %q has no anchor", id)
		}
	}
	for _, id := range ids {
		place, found := places[id]
		if !found {
			return nil, fmt.Errorf("load refreshed comment %q artifact: %w", id, pgx.ErrNoRows)
		}
		payload := model.CommentEventPayload{
			Comment: comments[id], ArtifactName: place.name, ProjectKey: place.projectKey, ArtifactSlug: place.slug,
		}
		// A body is bounded block by block, so it can be longer than one event carries: the event
		// takes its first part and says so, and a reader gets the rest from the comment itself.
		payload.Body, payload.BodyTruncated = model.EventBody(comments[id].Body)
		payloads[id] = payload
	}
	return payloads, nil
}

// anchorArtifactPlace is the name and place of the artifact a comment's anchor is on.
type anchorArtifactPlace struct{ name, projectKey, slug string }

// loadAnchorRefreshedComments reads each comment of ids with its mentions and deliveries, by id,
// and the place of the artifact each anchored comment's anchor is on, by comment id, for the
// comments whose anchor names an artifact that exists. Every id must name a comment. A deleted
// comment carries its delete mark (model.DeletedPostMark), as every other read of it does: the
// event this builds is stored as built, and a reader that takes the stored payload unmasked
// would otherwise read a comment with an empty body and no mark as one that stands.
func loadAnchorRefreshedComments(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.Comment, map[string]anchorArtifactPlace, error) {
	comments := make(map[string]model.Comment, len(ids))
	places := make(map[string]anchorArtifactPlace, len(ids))
	rows, err := tx.Query(ctx, `
		select c.id::text, c.issue_key, c.artifact_id::text, c.author, c.body, c.anchor, c.reply_to::text, c.ask_id::text, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at,
			c.deleted_at, c.deleted_by,
			aa.name, coalesce(aa.project_key, ''), aa.slug
		from comments c
		left join artifacts aa on aa.id = `+store.CastableUUID(`c.anchor->>'artifact_id'`)+`
		where c.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var comment model.Comment
		var author, anchor, resolvedBy, suggestion, deletedBy []byte
		var resolvedAt, editedAt, deletedAt *time.Time
		var anchorName, anchorSlug *string
		var anchorProject string
		if err := rows.Scan(
			&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
			&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt, &deletedAt, &deletedBy,
			&anchorName, &anchorProject, &anchorSlug,
		); err != nil {
			return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
		}
		id := comment.ID
		if comment.DeletedAt, comment.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
			return nil, nil, fmt.Errorf("refreshed comment %q: %w", id, err)
		}
		if err := json.Unmarshal(author, &comment.Author); err != nil {
			return nil, nil, fmt.Errorf("decode refreshed comment %q author: %w", id, err)
		}
		if len(anchor) > 0 {
			value := model.Anchor{}
			if err := model.DecodeAnchor(anchor, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q anchor: %w", id, err)
			}
			comment.Anchor = &value
		}
		if len(resolvedBy) > 0 {
			value := model.Actor{}
			if err := json.Unmarshal(resolvedBy, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q resolver: %w", id, err)
			}
			comment.ResolvedBy = &value
		}
		comment.ResolvedAt = askTimestamp(resolvedAt)
		comment.EditedAt = askTimestamp(editedAt)
		if len(suggestion) > 0 {
			value := model.Suggestion{}
			if err := json.Unmarshal(suggestion, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q suggestion: %w", id, err)
			}
			comment.Suggestion = &value
		}
		comment.Mentions = []model.Mention{}
		comment.Deliveries = []model.CommentDelivery{}
		comments[id] = comment
		if anchorName != nil {
			places[id] = anchorArtifactPlace{name: *anchorName, projectKey: anchorProject, slug: *anchorSlug}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if _, found := comments[id]; !found {
			return nil, nil, fmt.Errorf("load refreshed comment %q: %w", id, pgx.ErrNoRows)
		}
	}
	if err := loadAnchorRefreshedCommentMentions(ctx, tx, ids, comments); err != nil {
		return nil, nil, err
	}
	if err := loadAnchorRefreshedCommentDeliveries(ctx, tx, ids, comments); err != nil {
		return nil, nil, err
	}
	return comments, places, nil
}

// loadAnchorRefreshedCommentMentions appends each mention of the comments of ids to its comment
// in comments, in target order.
func loadAnchorRefreshedCommentMentions(ctx context.Context, tx pgx.Tx, ids []string, comments map[string]model.Comment) error {
	rows, err := tx.Query(ctx, `
		select comment_id::text, target, delivery, resolved_session_id
		from comment_mentions
		where comment_id = any($1::uuid[])
		order by comment_id, target
	`, ids)
	if err != nil {
		return fmt.Errorf("load refreshed comments' mentions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var commentID string
		mention := model.Mention{}
		if err := rows.Scan(&commentID, &mention.Target, &mention.Delivery, &mention.SessionID); err != nil {
			return fmt.Errorf("scan refreshed comment mention: %w", err)
		}
		comment := comments[commentID]
		comment.Mentions = append(comment.Mentions, mention)
		comments[commentID] = comment
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate refreshed comments' mentions: %w", err)
	}
	return nil
}

// loadAnchorRefreshedCommentDeliveries appends each delivery of the comments of ids to its
// comment in comments, in target and attempt order.
func loadAnchorRefreshedCommentDeliveries(ctx context.Context, tx pgx.Tx, ids []string, comments map[string]model.Comment) error {
	rows, err := tx.Query(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, duplicate, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = any($1::uuid[])
		order by comment_id, target, attempt
	`, ids)
	if err != nil {
		return fmt.Errorf("load refreshed comments' deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		delivery := model.CommentDelivery{}
		if err := rows.Scan(
			&delivery.CommentID, &delivery.Target, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID,
			&delivery.EnvelopeID, &delivery.Duplicate, &delivery.State, &delivery.Error, &delivery.ResolveError,
			&delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return fmt.Errorf("scan refreshed comment delivery: %w", err)
		}
		comment := comments[delivery.CommentID]
		comment.Deliveries = append(comment.Deliveries, delivery)
		comments[delivery.CommentID] = comment
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate refreshed comments' deliveries: %w", err)
	}
	return nil
}
