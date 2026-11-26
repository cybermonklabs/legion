package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// changedAnchor is an open anchor whose quote or orphaned flag its current tree mark changes.
// comment is whether its row is a comment's (a comment or a suggestion) rather than an ask's, and
// event builds the row's refresh event once the write phase reaches it.
type changedAnchor struct {
	mark      anchoredMark
	refreshed model.Anchor
	comment   bool
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

// refreshAnchors updates each open anchor from its current tree mark. Each persisted change
// appends the affected row's own full refresh event in this transaction; callers publish the
// collected committed events afterward. A comment whose anchor it orphans or finds again has its
// anchored_in retracted or stated under that event (stateRefreshedAnchoredIn).
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
func (s *Service) refreshAnchors(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, actor model.Actor) error {
	anchors, err := s.openAnchoredMarks(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	skippedMarkID, skipping := ctx.Value(skippedAnchorRefreshKey{}).(string)
	skipped := func(mark anchoredMark) bool { return skipping && mark.anchor.MarkID == skippedMarkID }
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
				ID: anchor.mark.id, Kind: MarkKind(anchor.mark.markType), Anchor: anchor.refreshed, Open: true,
			}
			if anchor.mark.pending != nil {
				row.Event = anchor.mark.pending.Event
			}
			ledger.RegisterPendingAnchor(row)
		}
	}
	anchoredIn, err := refreshedAnchoredIn(ctx, tx, changed, actor)
	if err != nil {
		return err
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
		for index, anchor := range changed {
			event, err := anchor.event()
			if err != nil {
				return err
			}
			if err := s.events.QueueAppend(b, event, sink); err != nil {
				return fmt.Errorf("append anchor refresh event: %w", err)
			}
			if tuples, held := anchoredIn[index]; held {
				stateRefreshedAnchoredIn(b, tuples)
			}
		}
		return nil
	})
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

// refreshedAnchoredIn is, by index into changed, the anchored_in and, for a comment that answers
// nothing, the part_of that keep each comment whose anchor the refresh orphans or finds again level
// with that anchor (graphstore.AnchorTuples, this issue's spec R31): a retraction of both to the
// block an anchor it orphans stood in, and both to the block an anchor it finds again stands in,
// each caused by that comment's own comment.anchor_refreshed, which the write phase appends right
// before them (graphstore.LatestEvent, stateRefreshedAnchoredIn). The block and whether the comment
// answers nothing are read off the comment's row in the read phase: the refresh's update writes
// only the anchor's quote and orphan flag, which neither reads, and the one writer of its block id,
// the backfill, takes the row's owner lock, which the refresh's version write holds
// (BackfillAnchorBlocks), so the row read here is the row the update leaves. A comment the store
// holds no node of, one this transaction has only queued, which the table does not hold yet, and a
// block past the store's bound, which is no node, state nothing; a change of the quote alone moves
// no block.
func refreshedAnchoredIn(ctx context.Context, tx pgx.Tx, changed []changedAnchor, actor model.Actor) (map[int][]graphstore.Tuple, error) {
	ids := make([]string, 0, len(changed))
	positions := make(map[string]int, len(changed))
	for index, anchor := range changed {
		if !anchor.comment || anchor.refreshed.Orphaned == anchor.mark.anchor.Orphaned {
			continue
		}
		ids = append(ids, anchor.mark.id)
		positions[anchor.mark.id] = index
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		select c.id::text, `+graphstore.AnchorBlockAddressSQL("c")+`, `+graphstore.CommentAnswersNothingSQL("c")+`
		from comments c
		where c.id = any($1::uuid[]) and c.node_kind is not null
		order by c.id
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the blocks of the refreshed comment anchors: %w", err)
	}
	defer rows.Close()
	now := time.Now().UTC()
	tuples := make(map[int][]graphstore.Tuple, len(ids))
	for rows.Next() {
		var id string
		var block *string
		var answersNothing bool
		if err := rows.Scan(&id, &block, &answersNothing); err != nil {
			return nil, fmt.Errorf("read the block of a refreshed comment anchor: %w", err)
		}
		if block != nil && graphstore.AddressFits(*block) {
			index := positions[id]
			tuples[index] = graphstore.AnchorTuples(nil, id, refs.BlockNodeKind, *block, answersNothing,
				!changed[index].refreshed.Orphaned, actor, now, graphstore.LatestEvent)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the blocks of the refreshed comment anchors: %w", err)
	}
	return tuples, nil
}

// stateRefreshedAnchoredIn queues on b the anchored_in and part_of refreshedAnchoredIn read of one
// refreshed comment, right after that comment's comment.anchor_refreshed, whose id they carry
// (graphstore.LatestEvent): nothing is appended between the two.
func stateRefreshedAnchoredIn(b *store.WriteBatch, tuples []graphstore.Tuple) {
	graphstore.QueueWrite(b, nil, tuples)
	graphstore.QueueHeads(b, graphstore.HeadsOf(nil, tuples))
}

// CommentAnchorInBlock is a comment's document anchor whose occurrence a read asks for
// (AnchorOccurrences): the id of the mark it names, the block it names, and its quote as stored.
type CommentAnchorInBlock struct {
	MarkID, BlockID, Quote string
}

// AnchorOccurrences is, by index into anchors, the zero-based occurrence of each comment's anchor
// on artifactID's document within the block it names (this issue's spec R31): the number of that
// block's matches of its quote that start before its mark, the occurrence
// pmdoc.QuoteFinder.FindInBlock takes for the mark (QuoteFinder.OccurrenceInBlock). The document is
// read as a read of its text reads it (documentTree), with no lock taken and no room loaded, and
// every mark is found in one walk of it (pmdoc.FindMarks). A comment's mark is a comment mark or,
// for a suggestion, a suggestion mark (commentMarkType), and an anchor names its id alone, so each
// is looked up as either. An anchor whose mark the document no longer holds, or holds outside the
// block it names, has no occurrence, nor has any anchor on a document with no state.
func (s *Service) AnchorOccurrences(ctx context.Context, artifactID string, anchors []CommentAnchorInBlock) (map[int]int, error) {
	tree, err := s.documentTree(ctx, artifactID)
	if err != nil || tree == nil {
		return nil, err
	}
	keys := make([]pmdoc.MarkRef, 0, 2*len(anchors))
	for _, anchor := range anchors {
		keys = append(keys, pmdoc.MarkRef{Type: string(MarkComment), ID: anchor.MarkID}, pmdoc.MarkRef{Type: string(MarkSuggestion), ID: anchor.MarkID})
	}
	found := pmdoc.FindMarks(tree, keys)
	finder := pmdoc.NewQuoteFinder(tree)
	occurrences := make(map[int]int, len(anchors))
	for index, anchor := range anchors {
		mark, held := found[keys[2*index]]
		if !held {
			if mark, held = found[keys[2*index+1]]; !held {
				continue
			}
		}
		occurrence, inside, err := finder.OccurrenceInBlock(anchor.Quote, anchor.BlockID, mark.Range.From)
		if err != nil {
			return nil, fmt.Errorf("count the matches of the quote of mark %s in block %s of %s: %w", anchor.MarkID, anchor.BlockID, artifactID, err)
		}
		if inside {
			occurrences[index] = occurrence
		}
	}
	return occurrences, nil
}

// NodeAnchorOrphans is what one write orphans of the node anchors on the nodes it writes
// (comments.node_anchor, this issue's spec R31): each comment whose node anchor names one of
// those nodes and is not orphaned, whose anchor the write marks orphaned, with the retraction of
// its anchored_in to that node and, for a comment that answers nothing, of its part_of to it
// (graphstore.AnchorTuples), each dated and signed by the write, at the sentinel. It is read in
// the write's read phase (ReadGoneNodeAnchors, RefreshNodeAnchors) and queued in its one batch
// (Queue). Its zero value orphans nothing.
type NodeAnchorOrphans struct {
	ids    []string
	issues []string
	tuples []graphstore.Tuple
}

// Issues is the issue of each comment o orphans, each once, in ascending key: the owners whose
// lock the write holds, so a caller confirms it locked each before it queues o.
func (o NodeAnchorOrphans) Issues() []string { return slices.Clone(o.issues) }

// Queue queues on b what o orphans: the orphaned mark of each comment's node anchor, written as
// the one key it decides, and the retractions, with the head rows they move
// (graphstore.QueueHeads). The comments' update moves no value a head row reads, so it keeps the
// issue's mark (graphstore.KeptHeadRows).
func (o NodeAnchorOrphans) Queue(b *store.WriteBatch) {
	if len(o.ids) == 0 {
		return
	}
	b.Queue("orphan comments node_anchor", `
		update comments set node_anchor = jsonb_set(node_anchor, '{orphaned}', to_jsonb(true))
		where id = any($1::uuid[]) and `+graphstore.KeptHeadRows, o.ids)
	if len(o.tuples) > 0 {
		graphstore.QueueWrite(b, nil, o.tuples)
		graphstore.QueueHeads(b, graphstore.HeadsOf(nil, o.tuples))
	}
}

// nodeAnchorsOnStatement is every comment whose node anchor names one of the nodes the kinds $1
// and the ids $2 pair up, as graph_nodes keys them, and is not orphaned: its id, its issue, the
// node, the quote and its occurrence, whether it answers nothing (graphstore.CommentAnswersNothingSQL),
// and whether the store holds its node, in id order. It reads comments_node_anchor_node
// (migration 20261125140001), whose expressions and predicate are its own.
var nodeAnchorsOnStatement = `
	select c.id::text, coalesce(c.issue_key, ''), c.node_anchor->>'node_kind', c.node_anchor->>'node_id',
	       coalesce(c.node_anchor->>'quote', ''), coalesce((c.node_anchor->>'occurrence')::int, 0),
	       ` + graphstore.CommentAnswersNothingSQL("c") + `, c.node_kind is not null
	from unnest($1::text[], $2::text[]) as named (kind, id)
	join comments c on c.node_anchor is not null
	  and (c.node_anchor->>'node_kind') = named.kind and (c.node_anchor->>'node_id') = named.id
	where not coalesce((c.node_anchor->>'orphaned')::boolean, false)
	order by c.id`

// nodeAnchored is one comment nodeAnchorsOnStatement reads.
type nodeAnchored struct {
	id, issue, kind, node, quote string
	occurrence                   int
	answersNothing, stored       bool
}

// readNodeAnchors reads, through q, every comment whose node anchor names one of nodes and is not
// orphaned (nodeAnchorsOnStatement).
func readNodeAnchors(ctx context.Context, q Queryer, nodes []graphstore.NodeKey) ([]nodeAnchored, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	kinds, ids := make([]string, len(nodes)), make([]string, len(nodes))
	for index, node := range nodes {
		kinds[index], ids[index] = node.Kind, node.ID
	}
	rows, err := q.Query(ctx, nodeAnchorsOnStatement, kinds, ids)
	if err != nil {
		return nil, fmt.Errorf("read the comments anchored on %d nodes: %w", len(nodes), err)
	}
	anchored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (nodeAnchored, error) {
		var comment nodeAnchored
		err := row.Scan(&comment.id, &comment.issue, &comment.kind, &comment.node, &comment.quote, &comment.occurrence,
			&comment.answersNothing, &comment.stored)
		return comment, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the comments anchored on %d nodes: %w", len(nodes), err)
	}
	return anchored, nil
}

// orphansOf is what orphaning each of anchored writes (NodeAnchorOrphans), dated at at and signed
// by who: a comment the store holds no node of retracts nothing.
func orphansOf(anchored []nodeAnchored, who model.Actor, at time.Time) NodeAnchorOrphans {
	var orphans NodeAnchorOrphans
	for _, comment := range anchored {
		orphans.ids = append(orphans.ids, comment.id)
		if comment.issue != "" {
			orphans.issues = append(orphans.issues, comment.issue)
		}
		if comment.stored {
			orphans.tuples = graphstore.AnchorTuples(orphans.tuples, comment.id, comment.kind, comment.node,
				comment.answersNothing, false, who, at, graphstore.NoEvent)
		}
	}
	slices.Sort(orphans.issues)
	orphans.issues = slices.Compact(orphans.issues)
	return orphans
}

// NodeAnchorIssues is the issue of each comment whose node anchor names one of nodes and is not
// orphaned, each once, in ascending key, read through q: the owners a write that takes those nodes
// gone locks beside its own before it reads them again (ReadGoneNodeAnchors), since a comment keeps
// its row's issue when a PATCH moves the node it is on under another issue.
func NodeAnchorIssues(ctx context.Context, q Queryer, nodes []graphstore.NodeKey) ([]string, error) {
	anchored, err := readNodeAnchors(ctx, q, nodes)
	if err != nil {
		return nil, err
	}
	return orphansOf(anchored, model.Actor{}, time.Time{}).Issues(), nil
}

// ReadGoneNodeAnchors is what the write that takes nodes gone - the node DELETE of this issue's
// spec R7 - orphans of the node anchors on them, in its own transaction (R31): every comment whose
// node anchor names one of nodes, as graph_nodes keys them, and is not orphaned, read through q in
// the write's read phase, dated at at and signed by who. The caller holds the owner lock of each
// issue its Issues names and queues it in its one batch.
func ReadGoneNodeAnchors(ctx context.Context, q Queryer, nodes []graphstore.NodeKey, who model.Actor, at time.Time) (NodeAnchorOrphans, error) {
	anchored, err := readNodeAnchors(ctx, q, nodes)
	if err != nil {
		return NodeAnchorOrphans{}, err
	}
	return orphansOf(anchored, who, at), nil
}

// RefreshNodeAnchors is the node-anchor refresh (this issue's spec R31): what the write that
// appends text, a new text value, to node orphans of the node anchors on that node, in the write's
// own transaction, as the refresh of document marks orphans a document anchor (refreshAnchors).
// Each comment whose node anchor names node and is not orphaned, read through q in the write's read
// phase, is orphaned where text no longer holds its quote at its occurrence (NodeTextHolds), dated
// at at and signed by who. No read orphans a node anchor: every read answers the stored mark. A
// lens, a finding, a risk and a constraint take their text once, from the route that creates them,
// and a result never changes (R12), so at this head no route calls it; each later write that
// appends such a text value calls it under the owner lock of each issue its Issues names and
// queues it in its one batch.
func RefreshNodeAnchors(ctx context.Context, q Queryer, node graphstore.NodeKey, text string, who model.Actor, at time.Time) (NodeAnchorOrphans, error) {
	anchored, err := readNodeAnchors(ctx, q, []graphstore.NodeKey{node})
	if err != nil {
		return NodeAnchorOrphans{}, err
	}
	dropped := anchored[:0]
	for _, comment := range anchored {
		if !NodeTextHolds(text, comment.quote, comment.occurrence) {
			dropped = append(dropped, comment)
		}
	}
	return orphansOf(dropped, who, at), nil
}

// NodeTextHolds is whether text, a node's text, holds quote at the zero-based occurrence
// (FindNodeQuote).
func NodeTextHolds(text, quote string, occurrence int) bool {
	if quote == "" || occurrence < 0 {
		return false
	}
	_, err := FindNodeQuote(text, quote, &occurrence)
	return err == nil
}

// FindNodeQuote is the zero-based occurrence of quote in text, a node's text (this issue's spec
// R31): the text read as markdown (pmdoc.Parse), as a block's text is, and the quote's matches in
// it counted as a quote beside block_id counts its matches in one block (pmdoc.QuoteFinder, R8):
// the occurrence named where one is, else the one match the text holds. A text that holds the
// quote nowhere, or not at that occurrence, is pmdoc's miss (pmdoc.ErrTargetNotFound); one that
// holds it in several places where none is named is its ambiguity (pmdoc.ErrTargetAmbiguous); a
// text that does not parse is the parse's error.
func FindNodeQuote(text, quote string, occurrence *int) (int, error) {
	doc, err := pmdoc.Parse(text)
	if err != nil {
		return 0, err
	}
	if _, err := pmdoc.FindQuote(doc, quote, occurrence, nil); err != nil {
		return 0, err
	}
	if occurrence != nil {
		return *occurrence, nil
	}
	return 0, nil
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
		left join artifacts ba on ba.id = `+refs.AskBlockDocumentSQL("a")+`
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
