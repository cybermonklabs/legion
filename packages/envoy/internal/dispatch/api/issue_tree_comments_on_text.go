package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Every answer of the tree read, with a view, with `kind` or with neither, carries each comment on
// the text of a node it returns, with every comment of that comment's thread (this issue's spec
// R8, R31): a comment R2's test passes as a node of the tree, and any other, an agent's comment no
// person answered among them, as context. A view marks each such comment as context. A comment on
// text comes only with the node it is on: no comment brings in a node. The comments an answer
// carries count toward its one page of at most 200, newest first, by each comment's own time.
//
// A comment on text is the first comment of a thread, one that answers nothing, whose current
// anchored_in at the read's cut names a node: a block, through a document anchor
// (comments.anchor), or a lens, a finding, a risk, a constraint or a result, through a node anchor
// (R31's comments.node_anchor); its thread is it and every comment whose reply_to chain climbs to
// it. The walk takes the comments R2's test passes (docs.WalkIssueTree); the read adds the others
// (placeTextComments), each under what it answers, the first under the node its anchored_in names,
// which keep lifts to that node's nearest ancestor the answer holds where no read returns it, as a
// paragraph of a role document.

// treeTextComment is one comment of a thread on text as the read holds it: the id of its thread's
// first comment, the one on text; the comment a reply answers, coalesce(answers, reply_to), zero on
// the first comment; when it was written, which pages it; and its current blocks, in their
// position order, read for a comment of a thread no person wrote in alone, which the walk does not
// take.
type treeTextComment struct {
	head    string
	answers refs.Target
	madeAt  time.Time
	blocks  []refs.Target
}

// treeTextThread is one thread on text: the node its first comment's anchored_in names (on); the
// node of the walk that names it (node), on itself or the node its moved_to tuples carry it to,
// zero where neither is one (resolveTextThreads); and whether a person wrote in it, R2's test of
// a comment's thread (voiced).
type treeTextThread struct {
	on, node refs.Target
	voiced   bool
}

// textCommentAnchoredIn is the far end of the current anchored_in of the comment c at the read's
// cut - the tuples at a seq up to $3 and, where $4 is not null, at a time up to $4, the newest to
// each far end, kept while it asserts - the newest of those, and no row where it holds none.
var textCommentAnchoredIn = `
			select anchoring.to_kind, anchoring.to_id from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.seq, t.asserted`,
	`t.from_kind = '`+graphstore.KindComment+`' and t.from_id = c.id::text and t.kind = '`+graphstore.TupleAnchoredIn+`'
				  and t.to_kind <> '`+graphstore.KindValue+`' and t.seq <= $3 and ($4::timestamptz is null or t.at <= $4)`) + `
			) anchoring
			where anchoring.asserted
			order by anchoring.seq desc limit 1`

// textCommentsStatement is every comment of the issue $1 of a thread on text at the cut: each
// first comment written by $2, one that answers nothing, whose current anchored_in names a node
// (textCommentAnchoredIn), found through its document anchor's document among the issue's
// documents (comments_anchor_artifact) or through its node anchor among the issue's comments that
// hold one (comments_issue_node_anchor), no comment holding both; and each comment written by $2
// whose reply_to chain climbs to one, its climb bounded at $5 links, as R2's test climbs a thread.
// Each row is the thread's first comment, the comment, when it was written, what it answers,
// whether a person wrote in its thread by $2 (R2's test: a comment of kind user), the node the
// first comment is on, and, of a thread no person wrote in, the comment's current blocks in their
// position order.
var textCommentsStatement = `
	with recursive heads as (
		select c.id, held.to_kind, held.to_id
		from artifacts a
		join comments c on (c.anchor->>'artifact_id')::uuid = a.id
		cross join lateral (` + textCommentAnchoredIn + `
		) held
		where a.issue_key = $1 and c.issue_key = $1 and c.created_at <= $2
		  and c.reply_to is null and c.answers is null and c.ask_id is null
		union all
		select c.id, held.to_kind, held.to_id
		from comments c
		cross join lateral (` + textCommentAnchoredIn + `
		) held
		where c.issue_key = $1 and c.node_anchor is not null and c.created_at <= $2
		  and c.reply_to is null and c.answers is null and c.ask_id is null
	),
	thread as (
		select h.id as head, h.id, 0 as depth from heads h
		union all
		select thread.head, c.id, thread.depth + 1
		from thread join comments c on c.reply_to = thread.id
		where thread.depth < $5 and c.created_at <= $2
	),
	members as (
		select thread.head, c.id, c.created_at, coalesce(c.answers, c.reply_to) as answers,
		       bool_or(coalesce(c.author->>'kind' = 'user', false)) over (partition by thread.head) as voiced
		from thread join comments c on c.id = thread.id
	)
	select m.head::text, m.id::text, m.created_at, m.answers::text, m.voiced, h.to_kind, h.to_id,
	       case when not m.voiced then array(
	         select b.node_id from comment_blocks b where b.comment_id = m.id and b.position is not null order by b.position) end
	from members m join heads h on h.id = m.head`

// olderCommentStatement is whether the issue $1 holds a comment written by $2 older than the
// position $3 and $4 names (refs.CommentPosition): a view's comments_before that no such comment
// follows is refused.
const olderCommentStatement = `
	select exists (
		select 1 from comments c
		where c.issue_key = $1 and c.created_at <= $2 and (c.created_at, c.id) < ($3::timestamptz, $4::uuid))`

// queueTextComments queues into batch the read of the issue's comments of threads on text at the
// read's cut (textCommentsStatement), into textComments, textThreads and textOrder, the last in
// the order each was written and then by id.
func (b *issueTreeBuilder) queueTextComments(batch *pgx.Batch) {
	seq, at := treeCutBounds(b.cut)
	b.textComments, b.textThreads = map[refs.Target]treeTextComment{}, map[string]*treeTextThread{}
	batch.Queue(textCommentsStatement, b.key, b.cut.At, seq, at, refs.IssueDepthCap).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var head, id, onKind, onID string
			var answers *string
			var voiced bool
			var blocks []string
			text := treeTextComment{}
			if err := rows.Scan(&head, &id, &text.madeAt, &answers, &voiced, &onKind, &onID, &blocks); err != nil {
				return fmt.Errorf("read a comment on the text of a node of %s: %w", b.key, err)
			}
			text.head = head
			if answers != nil && id != head {
				text.answers = refs.Target{Kind: graphstore.KindComment, ID: *answers}
			}
			for _, block := range blocks {
				text.blocks = append(text.blocks, refs.Target{Kind: refs.ItemComment.BlockKind(), ID: block})
			}
			target := refs.Target{Kind: graphstore.KindComment, ID: id}
			b.textComments[target] = text
			b.textOrder = append(b.textOrder, target)
			if _, held := b.textThreads[head]; !held {
				b.textThreads[head] = &treeTextThread{on: refs.Target{Kind: onKind, ID: onID}, voiced: voiced}
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read the comments on the text of the nodes of %s: %w", b.key, err)
		}
		slices.SortFunc(b.textOrder, func(left, right refs.Target) int {
			return cmp.Or(b.textComments[left].madeAt.Compare(b.textComments[right].madeAt), strings.Compare(left.ID, right.ID))
		})
		return nil
	})
}

// readViewTextComments reads, for a view, the issue's comments of threads on text
// (queueTextComments) and, beside comments_before, whether the issue holds a comment older than
// it, in one batch: a comments_before no older comment of the issue follows is 400 INVALID_QUERY.
func (b *issueTreeBuilder) readViewTextComments() error {
	batch := &pgx.Batch{}
	b.queueTextComments(batch)
	older := false
	if b.before != nil {
		batch.Queue(olderCommentStatement, b.key, b.cut.At, b.before.MadeAt, b.before.ID).QueryRow(func(row pgx.Row) error {
			return row.Scan(&older)
		})
	}
	if err := b.q.SendBatch(b.ctx, batch).Close(); err != nil {
		return fmt.Errorf("read the comments on the text of the nodes of %s: %w", b.key, err)
	}
	if b.before != nil && !older {
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%s", commentsBeforeRefusal(b.key))
	}
	return nil
}

// textOnSkeleton reports whether the read with no view can answer the thread's first comment by
// the node it is on: a block of a document whose blocks the skeleton holds, a finding of a lens,
// which the store keys as a block under its lens's address (graphstore.ParseFindingID), or a node
// of another kind. A comment on a block of any other document comes with no answer of the
// skeleton, as that block comes with none.
func textOnSkeleton(thread *treeTextThread, documents map[string]bool) bool {
	if thread.on.Kind != refs.BlockNodeKind {
		return true
	}
	if _, _, finding := graphstore.ParseFindingID(thread.on.ID); finding {
		return true
	}
	document, _, _ := strings.Cut(thread.on.ID, "#")
	return documents[document]
}

// commentOlder reports whether the comment written at madeAt whose id is id is older than before:
// written before it, or at its time with a lower id, a uuid comparing as its text does.
func commentOlder(madeAt time.Time, id string, before *refs.CommentPosition) bool {
	if before == nil {
		return true
	}
	return cmp.Or(madeAt.Compare(before.MadeAt), strings.Compare(id, before.ID)) < 0
}

// commentTimesStatement is when each comment $1 names was written.
const commentTimesStatement = `select c.id::text, c.created_at from comments c where c.id = any($1::uuid[])`

// mergeCommentPage is the comment page of the read with no view, and of the read with kind, out of
// page, the page of the issue's conversation comments older than the read's position (pastEnd
// where none is), and the comments of threads on text no person wrote in older than it whose node
// the skeleton can answer (textOnSkeleton): the issueTreeCommentPageSize newest of both, by each
// comment's own time and then its id, the conversation's in the page it answers, which the walk
// takes, and the others in textPage, which placeTextComments adds. Its Next names the oldest of them
// where older ones remain. The times of the conversation's comments it holds no row of are read in
// one statement. A position no older comment follows is 400 INVALID_QUERY.
func (b *issueTreeBuilder) mergeCommentPage(page refs.CommentPage, pastEnd bool, documents map[string]bool) (refs.CommentPage, error) {
	b.textPage = map[string]bool{}
	var candidates []refs.CommentPosition
	for _, target := range b.textOrder {
		text := b.textComments[target]
		thread := b.textThreads[text.head]
		if !thread.voiced && textOnSkeleton(thread, documents) && commentOlder(text.madeAt, target.ID, b.before) {
			candidates = append(candidates, refs.CommentPosition{MadeAt: text.madeAt, ID: target.ID})
		}
	}
	if len(candidates) == 0 {
		if pastEnd {
			return refs.CommentPage{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s", commentsBeforeRefusal(b.key))
		}
		return page, nil
	}
	merged := make([]refs.CommentPosition, 0, len(page.IDs)+len(candidates))
	var untimed []string
	for _, id := range page.IDs {
		if text, held := b.textComments[refs.Target{Kind: graphstore.KindComment, ID: id}]; held {
			merged = append(merged, refs.CommentPosition{MadeAt: text.madeAt, ID: id})
		} else {
			untimed = append(untimed, id)
		}
	}
	if len(untimed) > 0 {
		rows, err := b.q.Query(b.ctx, commentTimesStatement, untimed)
		if err != nil {
			return refs.CommentPage{}, fmt.Errorf("read when the comments of the page of %s were written: %w", b.key, err)
		}
		timed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (refs.CommentPosition, error) {
			var position refs.CommentPosition
			err := row.Scan(&position.ID, &position.MadeAt)
			return position, err
		})
		if err != nil {
			return refs.CommentPage{}, fmt.Errorf("read when the comments of the page of %s were written: %w", b.key, err)
		}
		merged = append(merged, timed...)
	}
	conversation := make(map[string]bool, len(page.IDs))
	for _, id := range page.IDs {
		conversation[id] = true
	}
	merged = append(merged, candidates...)
	slices.SortFunc(merged, func(left, right refs.CommentPosition) int {
		return cmp.Or(right.MadeAt.Compare(left.MadeAt), strings.Compare(right.ID, left.ID))
	})
	kept := merged[:min(issueTreeCommentPageSize, len(merged))]
	merging := refs.CommentPage{IDs: make([]string, 0, len(page.IDs))}
	for _, position := range kept {
		if conversation[position.ID] {
			merging.IDs = append(merging.IDs, position.ID)
		} else {
			b.textPage[position.ID] = true
		}
	}
	if len(merged) > len(kept) || page.Next != nil {
		oldest := kept[len(kept)-1]
		merging.Next = &oldest
	}
	return merging, nil
}

// resolveTextThreads gives each thread on text the node of the walk it is on (treeTextThread's
// node): the node its anchored_in names where the walk took it, else the first node of the walk
// its moved_to tuples carry that address to (refs.TreeMovedTo), read in one statement where the
// walk took none of the addresses.
func (b *issueTreeBuilder) resolveTextThreads() error {
	var unheld []string
	queued := map[string]bool{}
	for _, thread := range b.textThreads {
		if _, held := b.entries[thread.on]; held {
			thread.node = thread.on
		} else if thread.on.Kind == refs.BlockNodeKind && !queued[thread.on.ID] {
			queued[thread.on.ID] = true
			unheld = append(unheld, thread.on.ID)
		}
	}
	if len(unheld) == 0 {
		return nil
	}
	moved, err := refs.TreeMovedTo(b.ctx, b.q, unheld, b.cut)
	if err != nil {
		return err
	}
	for _, thread := range b.textThreads {
		if thread.node != (refs.Target{}) {
			continue
		}
		for _, reached := range moved[thread.on.ID] {
			if target := (refs.Target{Kind: refs.BlockNodeKind, ID: reached}); b.entries[target] != nil {
				thread.node = target
				break
			}
		}
	}
	return nil
}

// placeTextComments adds to the answer's candidates each comment of a thread on text no person
// wrote in, which the walk does not take: of a view, each of them; of the read with no view and
// the read with kind, those of the comment page (textPage). Each stands under what it answers,
// the first of its thread under the node of the walk it is on, and each of its blocks under it,
// each marked as on text (treeEntry's onText). A thread on a node the walk does not hold comes
// with no answer, and a reply whose comment the answer does not hold stands under the issue,
// naming that comment (named).
func (b *issueTreeBuilder) placeTextComments() error {
	if err := b.resolveTextThreads(); err != nil {
		return err
	}
	adding := map[refs.Target]bool{}
	var added []refs.Target
	for _, target := range b.textOrder {
		text := b.textComments[target]
		thread := b.textThreads[text.head]
		if thread.voiced || thread.node == (refs.Target{}) || b.skeleton != nil && !b.textPage[target.ID] {
			continue
		}
		adding[target] = true
		added = append(added, target)
	}
	for _, target := range added {
		text := b.textComments[target]
		parent := b.textThreads[text.head].node
		if text.answers != (refs.Target{}) {
			parent = text.answers
			if _, held := b.entries[parent]; !held && !adding[parent] {
				b.named[target] = parent
				parent = b.root()
			}
		}
		entry := b.add(target, parent, "")
		entry.typ, entry.madeAt, entry.onText = graphstore.KindComment, text.madeAt, true
		for _, block := range text.blocks {
			blockEntry := b.add(block, target, "")
			blockEntry.typ, blockEntry.madeAt, blockEntry.onText = block.Kind, text.madeAt, true
		}
	}
	return nil
}

// textHost is the node a thread on text stands with: the nearest node at or above the node of the
// walk it is on that the read with no view answers (kept), the node itself where the read answers
// it, and zero where it is on no node of the walk.
func (b *issueTreeBuilder) textHost(thread *treeTextThread) refs.Target {
	for at, steps := thread.node, 0; at != (refs.Target{}) && steps <= len(b.entries); steps++ {
		if b.kept[at] {
			return at
		}
		entry, held := b.entries[at]
		if !held {
			break
		}
		at = entry.parent
	}
	return refs.Target{}
}

// carriedText is, for a comment of a thread on text and for a block of one, the node its thread
// stands with (textHost), and whether the answer in holds that node: a comment on text comes only
// with the node it is on.
func (b *issueTreeBuilder) carriedText(target refs.Target, in map[refs.Target]bool) (refs.Target, bool) {
	comment := target
	if target.Kind == refs.ItemComment.BlockKind() {
		comment = b.entries[target].parent
	}
	text, held := b.textComments[comment]
	if !held {
		return refs.Target{}, false
	}
	host := b.textHost(b.textThreads[text.head])
	return host, host != (refs.Target{}) && in[host]
}

// pageViewComments is viewed, a view's nodes in its order, with the view's comment page alone of
// the comments on text it carries: the issueTreeCommentPageSize newest of those older than the
// read's position, by each comment's own time and then its id, each of the others left out with
// its blocks. It keeps the page in viewPage and, where older comments remain, the position of the
// oldest of it in viewNext.
func (b *issueTreeBuilder) pageViewComments(viewed []refs.Target) []refs.Target {
	var carried []refs.Target
	for _, target := range viewed {
		if _, held := b.textComments[target]; held && target.Kind == graphstore.KindComment {
			carried = append(carried, target)
		}
	}
	slices.SortFunc(carried, func(left, right refs.Target) int {
		return cmp.Or(b.textComments[right].madeAt.Compare(b.textComments[left].madeAt), strings.Compare(right.ID, left.ID))
	})
	b.viewPage = map[refs.Target]bool{}
	var oldest refs.Target
	remains := false
	for _, target := range carried {
		if !commentOlder(b.textComments[target].madeAt, target.ID, b.before) {
			continue
		}
		if len(b.viewPage) == issueTreeCommentPageSize {
			remains = true
			break
		}
		b.viewPage[target], oldest = true, target
	}
	if remains {
		b.viewNext = &refs.CommentPosition{MadeAt: b.textComments[oldest].madeAt, ID: oldest.ID}
	}
	return slices.DeleteFunc(viewed, func(target refs.Target) bool {
		comment := target
		if target.Kind == refs.ItemComment.BlockKind() {
			comment = b.entries[target].parent
		}
		_, onText := b.textComments[comment]
		return onText && comment.Kind == graphstore.KindComment && !b.viewPage[comment]
	})
}

// underPage is each node of order, an answer's nodes beside comments_before, that is a comment of
// its page or stands under one: of a view, a comment of its comment page (viewPage); of the read
// with kind, any comment, since the skeleton it filters holds the page's alone.
func (b *issueTreeBuilder) underPage(order []refs.Target) map[refs.Target]bool {
	members := make(map[refs.Target]bool, len(order))
	paged := func(target refs.Target) bool {
		return target.Kind == graphstore.KindComment && (b.view == "" || b.viewPage[target])
	}
	for _, target := range order {
		at := target
		for steps := 0; !paged(at) && steps <= len(b.entries); steps++ {
			entry, held := b.entries[at]
			if !held || at == b.root() {
				at = refs.Target{}
				break
			}
			at = entry.parent
		}
		if paged(at) {
			members[target] = true
		}
	}
	return members
}

// commentAnchorsStatement is the stored anchor of each comment $1 names that holds one: the
// document anchor of comments.anchor, which the comments read answers as model.Anchor, or the node
// anchor of comments.node_anchor (R31), beside the state of the node it names, read off that
// node's graph_nodes row by the store kind and id the anchor holds. No comment holds both.
const commentAnchorsStatement = `
	select c.id::text, c.anchor, c.node_anchor, n.state
	from comments c
	left join graph_nodes n on c.node_anchor is not null and n.kind = c.node_anchor->>'node_kind' and n.id = c.node_anchor->>'node_id'
	where c.id = any($1::uuid[]) and (c.anchor is not null or c.node_anchor is not null)`

// queueCommentAnchors queues into batch the read of the anchor of each of comments that holds one
// (commentAnchorsStatement), each as commentAnchorOf answers it, into anchors; and after it the
// climb from the block of each orphaned document anchor among them at the read's cut
// (anchorContainersStatement), which gives such an anchor its container (anchorContainers). A
// batch answers its statements in their order, so the climb's rows find the anchors read.
func (b *issueTreeBuilder) queueCommentAnchors(batch *pgx.Batch, comments []string) {
	b.anchors = map[string]*model.IssueTreeAnchor{}
	if len(comments) == 0 {
		return
	}
	batch.Queue(commentAnchorsStatement, comments).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var id string
			var document, node []byte
			var state *string
			if err := rows.Scan(&id, &document, &node, &state); err != nil {
				return err
			}
			anchor, err := commentAnchorOf(document, node, deref(state))
			if err != nil {
				return fmt.Errorf("decode the anchor of comment %s: %w", id, err)
			}
			b.anchors[id] = anchor
		}
		return rows.Err()
	})
	seq, at := treeCutBounds(b.cut)
	batch.Queue(anchorContainersStatement, comments, pmdoc.MaxTreeDepth, seq, at, refs.VisibilityArg(b.who)).Query(b.anchorContainers)
}

// anchorContainerCut bounds the tuples anchorContainersStatement reads to the read's cut: at a seq
// up to $3 and, where $4 is not null, at a time up to $4 (treeCutBounds).
const anchorContainerCut = `t.seq <= $3 and ($4::timestamptz is null or t.at <= $4)`

// anchorContainerValue is a lateral's body reading, at the read's cut (anchorContainerCut), the
// value of attribute out of the block climb.id, as the walk of the tree reads a node's value at a
// cut (refs.TreeCut): the newest tuple of that attribute by its time and then its seq, held where
// it asserts and null where it retracts, its first line alone where opening says so.
func anchorContainerValue(attribute string, opening bool) string {
	held := `t.to_id`
	if opening {
		held = `split_part(t.to_id, chr(10), 1)`
	}
	return `select case when t.asserted then ` + held + ` end as held
		from graph_tuples t
		where t.from_kind = '` + refs.BlockNodeKind + `' and t.from_id = climb.id and t.kind = '` + attribute + `'
		  and t.to_kind = '` + graphstore.KindValue + `' and ` + anchorContainerCut + `
		order by t.at desc, t.seq desc
		limit 1`
}

// anchorContainersStatement climbs, at the read's cut (anchorContainerCut), from the block that
// the orphaned document anchor of each comment $1 names (graphstore.AnchorBlockAddressSQL) up that
// block's part_of chain, as the walk of the tree reads a node's part_of at a cut (refs.TreeCut): of
// each block's part_of tuples, the newest to each far end by its time and then its seq, kept while
// it asserts, and of those the newest; the climb goes on while that names a block, at most $2
// links (pmdoc.MaxTreeDepth, the deepest a block stands in its document). It reads the chain in the
// store, so whatever the read answers - a view leaves the prose of a role document out - the chain
// is the block's own. Each row is a comment and a block of its climb, the anchor's block first,
// with that block's row's supertag and the first line of its text value at the cut, which say its
// type (refs.TreeNodeType); whether its state value at the cut is gone; and whether the reader $5
// may see it, the block kind's predicate over its row (refs.VisibleKindRow), true where the store
// holds no row of it, as the registry reads such a node. A comment's rows come in its climb's order.
var anchorContainersStatement = `
	with recursive climb (comment, id, depth) as (
		select c.id::text, anchored.id, 0
		from comments c
		cross join lateral (select ` + graphstore.AnchorBlockAddressSQL("c") + ` as id) anchored
		where c.id = any($1::uuid[]) and coalesce((c.anchor->>'orphaned')::boolean, false) and anchored.id is not null
		union all
		select climb.comment, placed.to_id, climb.depth + 1
		from climb
		cross join lateral (
			select held.to_kind, held.to_id from (
				select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
				from graph_tuples t
				where t.from_kind = '` + refs.BlockNodeKind + `' and t.from_id = climb.id and t.kind = '` + graphstore.TuplePartOf + `'
				  and t.to_kind <> '` + graphstore.KindValue + `' and ` + anchorContainerCut + `
				order by t.to_kind, t.to_id, t.at desc, t.seq desc
			) held
			where held.asserted
			order by held.at desc, held.seq desc
			limit 1
		) placed
		where placed.to_kind = '` + refs.BlockNodeKind + `' and climb.depth < $2
	)
	select climb.comment, climb.id, coalesce(node.supertag, ''), coalesce(opening.held, ''),
	       state.held is not distinct from '` + graphstore.StateGone + `',
	       coalesce(` + refs.VisibleKindRow(refs.BlockNodeKind, "node", "$5") + `, true)
	from climb
	left join lateral (` + anchorContainerValue(graphstore.AttributeText, true) + `) opening on true
	left join lateral (` + anchorContainerValue(graphstore.AttributeState, false) + `) state on true
	left join graph_nodes node on node.kind = '` + refs.BlockNodeKind + `' and node.id = climb.id
	order by climb.comment, climb.depth`

// anchorContainers gives each orphaned document anchor of anchors its container
// (model.IssueTreeAnchor's ContainerNodeID) off the rows of its climb (anchorContainersStatement):
// the first block of the climb that is a typed block at the read's cut, by the type the walk takes
// it by (refs.TreeNodeType), where no block of the climb up to it is gone there and the reader may
// see it. An anchor whose climb meets a gone block first, or whose nearest typed block the reader
// may not see, or whose climb holds no typed block, has none, and no block farther up stands in.
func (b *issueTreeBuilder) anchorContainers(rows pgx.Rows) error {
	settled := map[string]bool{}
	for rows.Next() {
		var comment, block, supertag, opening string
		var gone, seen bool
		if err := rows.Scan(&comment, &block, &supertag, &opening, &gone, &seen); err != nil {
			return fmt.Errorf("read a block above the anchor of a comment of %s: %w", b.key, err)
		}
		if settled[comment] {
			continue
		}
		typed := pmdoc.IsTypedBlock(refs.TreeNodeType(refs.BlockNodeKind, block, supertag, opening))
		settled[comment] = gone || typed
		if anchor := b.anchors[comment]; anchor != nil && typed && !gone && seen {
			anchor.ContainerNodeID = block
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the blocks above the anchors of the comments of %s: %w", b.key, err)
	}
	return nil
}

// commentAnchorOf is the anchor member of a comment the tree read answers (IssueTreeNode.Anchor),
// its orphaned mark as stored: from its document anchor, where it holds one, its artifact_id,
// block_id, mark_id, version, quote and orphaned, as the comments read decodes them
// (model.DecodeAnchor); else from its node anchor, the node it names with that node's state, its
// quote, its occurrence and orphaned.
func commentAnchorOf(document, node []byte, state string) (*model.IssueTreeAnchor, error) {
	if document != nil {
		var anchor model.Anchor
		if err := model.DecodeAnchor(document, &anchor); err != nil {
			return nil, err
		}
		return &model.IssueTreeAnchor{ArtifactID: anchor.ArtifactID, BlockID: anchor.BlockID, MarkID: anchor.MarkID, Version: anchor.Version,
			Quote: anchor.Quote, Orphaned: anchor.Orphaned}, nil
	}
	// A node anchor is decoded as the comment writer encodes it into comments.node_anchor (R31).
	var stored model.NodeAnchor
	if err := json.Unmarshal(node, &stored); err != nil {
		return nil, err
	}
	return &model.IssueTreeAnchor{Node: &model.IssueTreeAnchorNode{Kind: stored.NodeKind, ID: stored.NodeID, State: state},
		Quote: stored.Quote, Occurrence: &stored.Occurrence, Orphaned: stored.Orphaned}, nil
}

// anchorOccurrences gives each document anchor the read holds (anchors) that is not orphaned and
// names a block its zero-based occurrence within that block (R31): the number of the block's
// matches of its quote that start before its mark, the count pmdoc.QuoteFinder.FindInBlock takes,
// read off each document once (docs.Service.AnchorOccurrences). It runs after the read's
// transaction has closed, as documentPositions does, since a document's marks are its room's. An
// anchor whose document no longer holds its mark inside its block answers none, as an orphaned one
// does: a read-only page, which renders the document's Markdown without its marks, places a thread
// on the second of two identical quotes in one block only by this occurrence.
func (b *issueTreeBuilder) anchorOccurrences(s *server) error {
	asked := map[string][]docs.CommentAnchorInBlock{}
	held := map[string][]*model.IssueTreeAnchor{}
	for _, anchor := range b.anchors {
		if anchor.Node != nil || anchor.Orphaned || anchor.ArtifactID == "" || anchor.BlockID == nil || *anchor.BlockID == "" {
			continue
		}
		asked[anchor.ArtifactID] = append(asked[anchor.ArtifactID], docs.CommentAnchorInBlock{MarkID: anchor.MarkID, BlockID: *anchor.BlockID, Quote: anchor.Quote})
		held[anchor.ArtifactID] = append(held[anchor.ArtifactID], anchor)
	}
	for document, anchors := range asked {
		occurrences, err := s.deps.Docs.AnchorOccurrences(b.ctx, document, anchors)
		if err != nil {
			return fmt.Errorf("read the occurrences of the comments' anchors in document %s of %s: %w", document, b.key, err)
		}
		for index, occurrence := range occurrences {
			held[document][index].Occurrence = &occurrence
		}
	}
	return nil
}
