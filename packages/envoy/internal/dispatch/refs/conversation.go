package refs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// R2's test (dispatch://INFRA-1253/spec R2): which items of an issue are its conversation, the
// back-and-forth a person and the issue's agents had, and so may be a node's parent and are what
// a query of the issue's tree returns. An ask is always one. A comment or a message is one when a
// person wrote it, or when it sits in a thread a person wrote in. Any other comment or message an
// agent wrote - a status note, a round report, a handoff remark - is not, and neither is a message
// of the Legion daemon's own session, wherever it sits. A block of an item - a part of an ask, a
// block of a comment or of a message - is one when its item is.
//
// The test is read as of a time, so a version's walk reads it as it stood at that version: an
// agent's thread a person replies in later holds conversation nodes from the time of that reply
// on, and an older record keeps the answer it had. Only an item that stood at that time passes.
//
// A person is an author of kind `user`, as every read of a person's own writing tells one
// (api's answers, replies and unread reads). A thread a person wrote in is:
//   - for a comment, its thread: the comments of one head, the comment its reply_to chain climbs
//     to, or, for a comment in an ask's thread (ask_id), the comments of that ask; a person wrote
//     in an ask's thread too where the ask is theirs or they answered it;
//   - for a message, the messages of one root, the message its in_reply_to chain climbs to.
//
// IsConversationNode answers the test for one node, ConversationNodes for every item of an issue,
// and conversationTargets for the item blocks a write's pointers name (BlockTargets); all three
// read conversationItemIDs, so no two of them can disagree.

// daemonSessionPrefix starts the session id the Legion daemon writes an issue's status and
// lifecycle messages under, legion-daemon:<PROJECT> (packages/daemon's DaemonSession, and api's
// legionDaemonSessionPrefix, which keeps the daemon off an issue's collaborators).
const daemonSessionPrefix = "legion-daemon:"

// personAuthor is the SQL test of a stored author, the jsonb at column, being a person.
func personAuthor(column string) string {
	return `(` + column + `->>'kind' = 'user')`
}

// conversationKind is the item kind a node of kind names, with the item's uuid and, for a block
// of an item, the block's node id: an ask, a comment or a message by its own kind, and a block of
// one by its storage kind (ask_block, comment_block, message_block) or as the kind `block` every
// block node answers to (model.GraphNode.Kind), its id `<item kind>/<uuid>#<block id>`
// (ParseItemBlockNode). ok is false for every other kind and for an id that names no item uuid.
func conversationKind(kind, id string) (item ItemKind, itemID, blockNode string, ok bool) {
	switch kind {
	case string(ItemAsk), string(ItemComment), string(ItemMessage):
		item, itemID = ItemKind(kind), id
	case ItemAsk.BlockKind(), ItemComment.BlockKind(), ItemMessage.BlockKind(), "block":
		parsed, parsedItem, _, found := ParseItemBlockNode(id)
		if !found || (kind != "block" && parsed.BlockKind() != kind) {
			return "", "", "", false
		}
		item, itemID, blockNode = parsed, parsedItem, id
	default:
		return "", "", "", false
	}
	if canonical, held := canonicalUUID(itemID); !held || canonical != itemID {
		return "", "", "", false
	}
	return item, itemID, blockNode, true
}

// IsConversationNode reports whether the node kind/id is a conversation node at `at` (R2's test,
// above): an ask, a comment or a message that stood at `at`, or a block of one, by its storage
// kind or as a `block` node. An ask always is; a comment or a message is when a person wrote it
// or when a person had written in its thread by `at`; a message of the Legion daemon's session
// never is. A block is one when its item is and the item holds a block of that node id, current
// or removed. Any other kind, an id that names no item, and an item that did not stand at `at`
// answer false.
func IsConversationNode(ctx context.Context, q Queryer, kind, id string, at time.Time) (bool, error) {
	item, itemID, blockNode, ok := conversationKind(kind, id)
	if !ok {
		return false, nil
	}
	if blockNode != "" {
		tables, err := item.tables()
		if err != nil {
			return false, err
		}
		var held bool
		if err := q.QueryRow(ctx, `select exists(select 1 from `+tables.blocks+` where node_id = $1)`, blockNode).Scan(&held); err != nil {
			return false, fmt.Errorf("read block %s: %w", blockNode, err)
		}
		if !held {
			return false, nil
		}
	}
	passing, err := conversationItemIDs(ctx, q, item, []string{itemID}, at)
	if err != nil {
		return false, err
	}
	return passing[itemID], nil
}

// ConversationNodes is the conversation of issue issueKey as it stood at `at`, for who: each of
// the issue's asks, comments and messages that stood at `at` and that R2's test passes at `at`,
// in the order they were written (created_at, then kind and id), each followed by its current
// blocks in their order, every node loaded as nodeRegistry's loaders load it (loadAskNodes,
// loadCommentNodes, loadMessageNodes and the item block loaders), with the parent its loader
// names and the excerpt an edge at it shows, as ExcerptedNodes answers each. A node who may not
// see is left out, as every loader leaves it out, and so are the blocks of an item left out.
func ConversationNodes(ctx context.Context, q Queryer, who model.Actor, issueKey string, at time.Time) ([]ExcerptedNode, error) {
	return conversationNodes(ctx, q, who, issueKey, at, nil)
}

// ConversationPageNodes is ConversationNodes with the comments of page alone, which R2's test
// passed as page was read (ReadCommentPage): the asks and the messages of the issue, and those
// comments.
func ConversationPageNodes(ctx context.Context, q Queryer, who model.Actor, issueKey string, at time.Time, page CommentPage) ([]ExcerptedNode, error) {
	return conversationNodes(ctx, q, who, issueKey, at, &page)
}

// conversationItemsStatement lists an issue's items, its comments those comments selects, as c.
func conversationItemsStatement(comments string) string {
	return `
		select kind, id from (
			select 'ask' as kind, id::text as id, created_at from asks where issue_key = $1 and created_at <= $2
			union all
			select 'comment', c.id::text, c.created_at from ` + comments + `
			union all
			select 'message', id::text, created_at from messages where issue_key = $1 and created_at <= $2
		) items
		order by created_at, kind, id
	`
}

// conversationItems lists every item of the issue $1 written by $2, and conversationPageItems its
// asks and messages and the comments of the page $3, each comment read by its key: read as a
// filter over the issue's range of comments_issue_created, a page of 200 read all 9,252 comments
// of its issue.
var (
	conversationItems     = conversationItemsStatement(`comments c where c.issue_key = $1 and c.created_at <= $2`)
	conversationPageItems = conversationItemsStatement(`unnest($3::uuid[]) page(id)
			cross join lateral (
				select c.id, c.created_at from comments c where c.id = page.id and c.issue_key = $1 and c.created_at <= $2
				offset 0) c`)
)

// conversationNodes is ConversationNodes, its comments those of page where it is set.
func conversationNodes(ctx context.Context, q Queryer, who model.Actor, issueKey string, at time.Time, page *CommentPage) ([]ExcerptedNode, error) {
	statement, args := conversationItems, []any{issueKey, at}
	if page != nil {
		statement, args = conversationPageItems, append(args, page.IDs)
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list the items of %s: %w", issueKey, err)
	}
	var written []edgeRow
	ids := map[ItemKind][]string{}
	for rows.Next() {
		var item edgeRow
		if err := rows.Scan(&item.otherKind, &item.otherID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan an item of %s: %w", issueKey, err)
		}
		written = append(written, item)
		ids[ItemKind(item.otherKind)] = append(ids[ItemKind(item.otherKind)], item.otherID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the items of %s: %w", issueKey, err)
	}

	passing := map[ItemKind]map[string]bool{}
	for kind, of := range ids {
		if page != nil && kind == ItemComment {
			passing[kind] = make(map[string]bool, len(of))
			for _, id := range of {
				passing[kind][id] = true
			}
			continue
		}
		if passing[kind], err = conversationItemIDs(ctx, q, kind, of, at); err != nil {
			return nil, err
		}
	}
	kept := slices.DeleteFunc(written, func(item edgeRow) bool { return !passing[ItemKind(item.otherKind)][item.otherID] })
	if len(kept) == 0 {
		return []ExcerptedNode{}, nil
	}
	blocks, err := conversationBlocks(ctx, q, kept)
	if err != nil {
		return nil, err
	}
	loading := slices.Clone(kept)
	for _, item := range kept {
		loading = append(loading, blocks[[2]string{item.otherKind, item.otherID}]...)
	}
	resolved, err := resolveNodes(ctx, q, who, false, loading)
	if err != nil {
		return nil, err
	}
	nodes := make([]ExcerptedNode, 0, len(loading))
	for _, item := range kept {
		node, found := resolved[[2]string{item.otherKind, item.otherID}]
		if !found {
			continue
		}
		nodes = append(nodes, ExcerptedNode{Node: node.GraphNode, Excerpt: excerptOf(node)})
		for _, block := range blocks[[2]string{item.otherKind, item.otherID}] {
			if node, found := resolved[[2]string{block.otherKind, block.otherID}]; found {
				nodes = append(nodes, ExcerptedNode{Node: node.GraphNode, Excerpt: excerptOf(node)})
			}
		}
	}
	return nodes, nil
}

// conversationBlocks is the current blocks of each item of items, by the item's kind and id, in
// their order in the item, each as the row resolveNodes loads it by: its storage kind and node id.
func conversationBlocks(ctx context.Context, q Queryer, items []edgeRow) (map[[2]string][]edgeRow, error) {
	byKind := map[ItemKind][]string{}
	for _, item := range items {
		byKind[ItemKind(item.otherKind)] = append(byKind[ItemKind(item.otherKind)], item.otherID)
	}
	blocks := map[[2]string][]edgeRow{}
	for _, kind := range []ItemKind{ItemAsk, ItemComment, ItemMessage} {
		if len(byKind[kind]) == 0 {
			continue
		}
		tables, err := kind.tables()
		if err != nil {
			return nil, err
		}
		rows, err := q.Query(ctx, `
			select `+tables.item+`::text, node_id from `+tables.blocks+`
			where `+tables.item+` = any($1::uuid[]) and position is not null
			order by `+tables.item+`, position
		`, byKind[kind])
		if err != nil {
			return nil, fmt.Errorf("list the blocks of the %ss of a conversation: %w", kind, err)
		}
		for rows.Next() {
			var itemID, node string
			if err := rows.Scan(&itemID, &node); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan a block of a %s: %w", kind, err)
			}
			key := [2]string{string(kind), itemID}
			blocks[key] = append(blocks[key], edgeRow{otherKind: kind.BlockKind(), otherID: node})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list the blocks of the %ss of a conversation: %w", kind, err)
		}
	}
	return blocks, nil
}

// conversationItemIDs is the set of ids among ids, each the uuid of an item of kind, that R2's
// test passes at `at`: an ask that stood at `at`; a comment or a message that stood at `at`,
// that a person wrote, or whose thread a person had written in by `at`; never a message of the
// Legion daemon's session. An id that is no uuid, or that names no item, is not in the set.
func conversationItemIDs(ctx context.Context, q Queryer, kind ItemKind, ids []string, at time.Time) (map[string]bool, error) {
	passing := make(map[string]bool, len(ids))
	subjects := make([]string, 0, len(ids))
	for _, id := range ids {
		if canonical, held := canonicalUUID(id); held && canonical == id {
			subjects = append(subjects, id)
		}
	}
	if len(subjects) == 0 {
		return passing, nil
	}
	statement, args := "", []any{subjects, at, IssueDepthCap}
	switch kind {
	case ItemAsk:
		statement, args = `select id::text from asks where id = any($1::uuid[]) and created_at <= $2`, args[:2]
	case ItemComment:
		statement = commentConversationStatement
	case ItemMessage:
		statement = messageConversationStatement
	default:
		return nil, fmt.Errorf("test the conversation of a %q: not an item kind", string(kind))
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("test which %ss are a conversation: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a %s of a conversation: %w", kind, err)
		}
		passing[id] = true
	}
	return passing, rows.Err()
}

// commentConversationStatement is R2's test over the comments $1 names, at $2, its walks bounded
// at $3 links, as the comment create's thread climb is (api's threadHeadFrom): each comment's
// thread is the top of its reply_to chain - the ask its chain reaches, else its head - and a
// person wrote in that thread by $2 when one of its comments written by then is a person's, or,
// for an ask's thread, the ask is a person's or a person had answered it by then.
var commentConversationStatement = commentConversationSQL(`c.id = any($1::uuid[])`)

// commentsOffPageStatement is commentConversationStatement over the comments $1 names that are
// comments of the issue $4.
var commentsOffPageStatement = commentConversationSQL(`c.id = any($1::uuid[]) and c.issue_key = $4`)

// commentPageSubjects is the comments of the issue $1 a page reads: every one where $4 is null,
// else those older than the position $4 and $5 name (CommentPosition), a uuid sorting as its text
// does.
const commentPageSubjects = `c.issue_key = $1 and ($4::timestamptz is null or (c.created_at, c.id) < ($4::timestamptz, $5::uuid))`

// commentPageStatement is the $6 newest comments of the issue $1 that R2's test passes, as
// commentConversationStatement holds it, of those commentPageSubjects reads: each with when it was
// written, newest first and then by id. The comments a person wrote are read newest first through
// comments_issue_created, as far as the page reaches.
var commentPageStatement = commentConversationThreads(commentPageSubjects) + `
	select passing.id::text, passing.created_at
	from (
		(select c.id, c.created_at
		 from comments c
		 where ` + commentPageSubjects + ` and c.created_at <= $2 and ` + personAuthor("c.author") + `
		 order by c.created_at desc, c.id desc
		 limit $6)
		union all
		select c.id, c.created_at
		from top t join comments c on c.id = t.subject
		where coalesce(t.ask_id, t.head) in (select thread from voiced)
	) passing
	order by passing.created_at desc, passing.id desc
	limit $6`

// commentConversationSQL is R2's test over the comments the condition subjects selects: the id of
// each comment that passes. A comment a person wrote passes as it is read; only the others climb
// to their threads (commentConversationThreads).
func commentConversationSQL(subjects string) string {
	return commentConversationThreads(subjects) + `
	select c.id::text
	from comments c
	where ` + subjects + ` and c.created_at <= $2 and ` + personAuthor("c.author") + `
	union all
	select c.id::text
	from top t join comments c on c.id = t.subject
	where coalesce(t.ask_id, t.head) in (select thread from voiced)`
}

// commentConversationThreads is the threads of the comments the condition subjects selects that
// no person wrote, written by $2 - top, each such comment with its thread - and voiced, each of
// those threads a person wrote in by then. A comment a person wrote climbs to no thread, so an
// issue whose comments are a person's walks none: the walk of every comment of an issue of 9,252
// cost its read half its time.
func commentConversationThreads(subjects string) string {
	return `
	with recursive up as (
		select c.id as subject, c.id, c.reply_to, c.ask_id, 0 as depth
		from comments c where ` + subjects + ` and c.created_at <= $2 and ` + personAuthor("c.author") + ` is not true
		union all
		select up.subject, p.id, p.reply_to, p.ask_id, up.depth + 1
		from up join comments p on p.id = up.reply_to
		where up.ask_id is null and up.depth < $3
	),
	top as (
		select distinct on (subject) subject, id as head, ask_id
		from up order by subject, depth desc
	),
	threads as (
		select distinct on (coalesce(ask_id, head)) head, ask_id from top
		order by coalesce(ask_id, head), head
	),
	member as (
		select t.head, t.ask_id, c.id, 0 as depth
		from threads t join comments c on (t.ask_id is null and c.id = t.head) or (t.ask_id is not null and c.ask_id = t.ask_id)
		union all
		select member.head, member.ask_id, c.id, member.depth + 1
		from member join comments c on c.reply_to = member.id
		where member.depth < $3
	),
	voiced as (
		select coalesce(m.ask_id, m.head) as thread
		from member m join comments c on c.id = m.id
		where ` + personAuthor("c.author") + ` and c.created_at <= $2
		union
		select t.ask_id
		from threads t join asks a on a.id = t.ask_id
		where (` + personAuthor("a.author") + ` and a.created_at <= $2)
		   or (a.answer->>'user' is not null and (a.answer->>'at')::timestamptz <= $2)
	)`
}

// CommentPosition is a place in the order of an issue's comments, newest first: a comment's
// made_at and id, which the read with no view answers as `comments_before` and reads back from it
// (String, ParseCommentPosition). The comments older than it are those written before MadeAt, and
// those written at MadeAt whose id is lower.
type CommentPosition struct {
	MadeAt time.Time
	ID     string
}

// commentPositionLayout is a position's made_at: RFC 3339, in UTC, with microseconds, the
// precision Postgres keeps.
const commentPositionLayout = "2006-01-02T15:04:05.000000Z"

// CommentPositionShape is how a position is written, for the refusal of one written otherwise.
const CommentPositionShape = "<made_at>,<id>"

// String is the position written `<made_at>,<id>`.
func (position CommentPosition) String() string {
	return position.MadeAt.UTC().Format(commentPositionLayout) + "," + position.ID
}

// ParseCommentPosition reads a position written as String writes it, and false for any other
// text: a made_at of another form, or an id that is no canonical uuid.
func ParseCommentPosition(written string) (CommentPosition, bool) {
	madeAt, id, cut := strings.Cut(written, ",")
	if !cut {
		return CommentPosition{}, false
	}
	at, err := time.Parse(commentPositionLayout, madeAt)
	if err != nil {
		return CommentPosition{}, false
	}
	if canonical, held := canonicalUUID(id); !held || canonical != id {
		return CommentPosition{}, false
	}
	return CommentPosition{MadeAt: at, ID: id}, true
}

// CommentPage is one page of an issue's conversation comments, newest first: the ids of its
// comments, and Next, the position of its oldest comment where older ones remain, nil where none
// does. Every other comment of the conversation, newer or older, is off the page
// (CommentsOffPage).
type CommentPage struct {
	IDs  []string
	Next *CommentPosition
}

// ErrCommentPositionPastEnd is a page asked for past a position that no older comment follows.
var ErrCommentPositionPastEnd = errors.New("no comment of the issue is older than the position")

// ReadCommentPage is the page of the issue issueKey's comments that R2's test passes at `at` (its
// conversation), at most size of them, newest first by when each was written and then by id: the
// newest where before is nil, else those older than before (CommentPosition), and
// ErrCommentPositionPastEnd where none is. It reads the page and the comment after it alone.
func ReadCommentPage(ctx context.Context, q Queryer, issueKey string, at time.Time, before *CommentPosition, size int) (CommentPage, error) {
	var beforeAt *time.Time
	var beforeID *string
	if before != nil {
		beforeAt, beforeID = &before.MadeAt, &before.ID
	}
	rows, err := q.Query(ctx, commentPageStatement, issueKey, at, IssueDepthCap, beforeAt, beforeID, size+1)
	if err != nil {
		return CommentPage{}, fmt.Errorf("read the comments of %s: %w", issueKey, err)
	}
	var comments []CommentPosition
	for rows.Next() {
		var comment CommentPosition
		if err := rows.Scan(&comment.ID, &comment.MadeAt); err != nil {
			rows.Close()
			return CommentPage{}, fmt.Errorf("scan a comment of %s: %w", issueKey, err)
		}
		comments = append(comments, comment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CommentPage{}, fmt.Errorf("read the comments of %s: %w", issueKey, err)
	}
	if before != nil && len(comments) == 0 {
		return CommentPage{}, ErrCommentPositionPastEnd
	}
	slices.SortFunc(comments, func(left, right CommentPosition) int {
		return cmp.Or(right.MadeAt.Compare(left.MadeAt), strings.Compare(right.ID, left.ID))
	})
	page := CommentPage{IDs: make([]string, 0, min(size, len(comments)))}
	for index, comment := range comments {
		if index == size {
			oldest := comments[index-1]
			page.Next = &oldest
			break
		}
		page.IDs = append(page.IDs, comment.ID)
	}
	return page, nil
}

// CommentsOffPage is each of ids, a comment's uuid, that is a comment of the issue issueKey that
// R2's test passes at `at`, as ReadCommentPage reads them, and is not on page: a comment of
// another page of the conversation. An id that is no uuid, or that names no such comment, is not
// in it.
func CommentsOffPage(ctx context.Context, q Queryer, issueKey string, at time.Time, page CommentPage, ids []string) (map[string]bool, error) {
	off := map[string]bool{}
	subjects := make([]string, 0, len(ids))
	for _, id := range ids {
		if canonical, held := canonicalUUID(id); held && canonical == id && !slices.Contains(page.IDs, id) {
			subjects = append(subjects, id)
		}
	}
	if len(subjects) == 0 {
		return off, nil
	}
	rows, err := q.Query(ctx, commentsOffPageStatement, subjects, at, IssueDepthCap, issueKey)
	if err != nil {
		return nil, fmt.Errorf("read which comments of %s are of another page: %w", issueKey, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a comment of %s of another page: %w", issueKey, err)
		}
		off[id] = true
	}
	return off, rows.Err()
}

// messageConversationStatement is R2's test over the messages $1 names, at $2, its walks bounded
// at $3 links: each message's thread is the root its in_reply_to chain climbs to (api's
// messageThreadCTE), and a person wrote in it by $2 when one of its messages written by then is a
// person's. A message of the Legion daemon's session passes never.
var messageConversationStatement = `
	with recursive up as (
		select m.id as subject, m.id, m.in_reply_to, 0 as depth
		from messages m where m.id = any($1::uuid[]) and m.created_at <= $2
		union all
		select up.subject, p.id, p.in_reply_to, up.depth + 1
		from up join messages p on p.id = up.in_reply_to
		where up.depth < $3
	),
	top as (
		select distinct on (subject) subject, id as root
		from up order by subject, depth desc
	),
	member as (
		select roots.root, roots.root as id, 0 as depth from (select distinct root from top) roots
		union all
		select member.root, m.id, member.depth + 1
		from member join messages m on m.in_reply_to = member.id
		where member.depth < $3
	),
	voiced as (
		select distinct member.root
		from member join messages m on m.id = member.id
		where ` + personAuthor("m.author") + ` and m.created_at <= $2
	)
	select t.subject::text
	from top t join messages m on m.id = t.subject
	where not (m.author->>'kind' = 'session' and starts_with(m.author->>'id', '` + daemonSessionPrefix + `'))
	  and (` + personAuthor("m.author") + ` or t.root in (select root from voiced))`

// conversationTargets is each ask, comment or message targets names, itself or a block of it,
// that stands now, by node id (text.TargetID: an item's uuid, a block's `<kind>/<uuid>#<id>`), as
// a write's pointer resolution (BlockTargets) reads it: its item's own issue, never the one a
// reference spells, and whether it is a conversation node at `at`, the write's time. An item no
// row of an issue holds, and a block no item holds now, is absent.
func conversationTargets(ctx context.Context, q Queryer, targets []text.Ref, at time.Time) (map[string]pmdoc.BlockTarget, error) {
	found := map[string]pmdoc.BlockTarget{}
	var blocks []string
	whole := map[ItemKind][]string{}
	for _, target := range targets {
		switch kind := pmdoc.ItemKindOf(target); {
		case kind == "":
		case target.Kind == "block":
			blocks = append(blocks, text.TargetID(target))
		default:
			if canonical, held := canonicalUUID(target.ID); held && canonical == target.ID {
				whole[ItemKind(kind)] = append(whole[ItemKind(kind)], target.ID)
			}
		}
	}
	if len(blocks) == 0 && len(whole) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select 'comment', b.node_id, c.id::text, c.issue_key from comment_blocks b join comments c on c.id = b.comment_id
		where b.node_id = any($1) and b.position is not null and c.issue_key is not null
		union all
		select 'message', b.node_id, m.id::text, m.issue_key from message_blocks b join messages m on m.id = b.message_id
		where b.node_id = any($1) and b.position is not null and m.issue_key is not null
		union all
		select 'ask', b.node_id, a.id::text, a.issue_key from ask_blocks b join asks a on a.id = b.ask_id
		where b.node_id = any($1) and b.position is not null and a.issue_key is not null
		union all
		select 'comment', c.id::text, c.id::text, c.issue_key from comments c where c.id = any($2::uuid[]) and c.issue_key is not null
		union all
		select 'message', m.id::text, m.id::text, m.issue_key from messages m where m.id = any($3::uuid[]) and m.issue_key is not null
		union all
		select 'ask', a.id::text, a.id::text, a.issue_key from asks a where a.id = any($4::uuid[]) and a.issue_key is not null
	`, blocks, whole[ItemComment], whole[ItemMessage], whole[ItemAsk])
	if err != nil {
		return nil, fmt.Errorf("read the items and item blocks a write points at: %w", err)
	}
	items := map[ItemKind][]string{}
	owner := map[string][2]string{}
	for rows.Next() {
		var kind, node, itemID, issue string
		if err := rows.Scan(&kind, &node, &itemID, &issue); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan an item or item block a write points at: %w", err)
		}
		found[node] = pmdoc.BlockTarget{Issue: issue}
		items[ItemKind(kind)] = append(items[ItemKind(kind)], itemID)
		owner[node] = [2]string{kind, itemID}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the items and item blocks a write points at: %w", err)
	}
	for kind, ids := range items {
		passing, err := conversationItemIDs(ctx, q, kind, ids, at)
		if err != nil {
			return nil, err
		}
		for node, item := range owner {
			if item[0] == string(kind) && passing[item[1]] {
				target := found[node]
				target.Conversation = true
				found[node] = target
			}
		}
	}
	return found, nil
}
