package refs

import (
	"context"
	"fmt"
	"slices"
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
// names. A node who may not see is left out, as every loader leaves it out, and so are the blocks
// of an item left out.
func ConversationNodes(ctx context.Context, q Queryer, who model.Actor, issueKey string, at time.Time) ([]model.GraphNode, error) {
	rows, err := q.Query(ctx, `
		select kind, id from (
			select 'ask' as kind, id::text as id, created_at from asks where issue_key = $1 and created_at <= $2
			union all
			select 'comment', id::text, created_at from comments where issue_key = $1 and created_at <= $2
			union all
			select 'message', id::text, created_at from messages where issue_key = $1 and created_at <= $2
		) items
		order by created_at, kind, id
	`, issueKey, at)
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
		if passing[kind], err = conversationItemIDs(ctx, q, kind, of, at); err != nil {
			return nil, err
		}
	}
	kept := slices.DeleteFunc(written, func(item edgeRow) bool { return !passing[ItemKind(item.otherKind)][item.otherID] })
	if len(kept) == 0 {
		return []model.GraphNode{}, nil
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
	nodes := make([]model.GraphNode, 0, len(loading))
	for _, item := range kept {
		node, found := resolved[[2]string{item.otherKind, item.otherID}]
		if !found {
			continue
		}
		nodes = append(nodes, node.GraphNode)
		for _, block := range blocks[[2]string{item.otherKind, item.otherID}] {
			if node, found := resolved[[2]string{block.otherKind, block.otherID}]; found {
				nodes = append(nodes, node.GraphNode)
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
var commentConversationStatement = `
	with recursive up as (
		select c.id as subject, c.id, c.reply_to, c.ask_id, 0 as depth
		from comments c where c.id = any($1::uuid[]) and c.created_at <= $2
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
	)
	select t.subject::text
	from top t join comments c on c.id = t.subject
	where ` + personAuthor("c.author") + ` or coalesce(t.ask_id, t.head) in (select thread from voiced)`

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
