package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Query selects one node's edges. Direction "in" reads edges pointing at the node (Kind, ID
// are its to_kind, to_id: ref_key for artifacts); "out" reads edges it writes (from_kind,
// from_id: the uuid for artifacts). Kinds narrows edge types; Since keeps mentions whose
// source_seq is greater, and Through those whose source_seq is at or below it; either excludes
// every structural edge.
//
// A document's read also lists the edges at its current blocks: on "in", every edge pointing at
// one of them, and on "out", every link one of them holds; each such edge names that block as
// GraphEdge.Block. A comment's or a message's read does the same with the item's blocks. A
// document's, comment's or message's own mention of a target one of its blocks also cites is
// listed from that block alone (heldByABlock), so each link shows once, where its block holds it.
// A reply that names a current block of the item read is listed once, under that block: its
// replies_to edge to the item itself (a comment's thread root, the message a message answers) is
// not listed.
// A reply whose answers names the comment its replies_to names is listed once, by replies_to,
// wherever replies_to is listed.
type Query struct {
	// ServerURL is the deployment the caller reads, whose dashboard URLs an edge's excerpt is cut
	// around as references (itemExcerpt).
	ServerURL string
	Direction string
	Kind      string
	ID        string
	Kinds     []string
	// ExcludeKinds drops edge kinds the caller never means by "referenced by": an ask's
	// inbound `replies_to` edges are its own clarification thread, which its card renders.
	ExcludeKinds []string
	Since        *int64
	// Through bounds a Since read at a safe horizon of the event log (events.Horizon), at or
	// below which every event is final: event ids are not commit order, so a caller that moved
	// its since past a returned edge's source_seq would never be shown an edge whose event
	// drew a lower id and committed later.
	Through *int64
	// WholeDocuments, on a document's "in" read, lists the items and documents that cite it or
	// one of its current blocks, and no block: a citing document is listed by its own edges,
	// and an ask that cites it from a part by its ask row (asksCitingTheDocument).
	WholeDocuments bool
}

// AskBacklinkExclusions is what "referenced by" never means for an ask: its own clarification
// thread, which every ask surface renders inline, and its own parts, each part_of it, which are
// the ask's own text. The count on a card and the rows its panel opens read the same exclusion.
var AskBacklinkExclusions = []string{"replies_to", "part_of"}

// resolvedEdge is a graph edge plus the ref_key of an artifact node, which the legacy
// referenced-by shape still reports.
type resolvedEdge struct {
	model.GraphEdge
	refKey string
}

type edgeRow struct {
	kind      string
	otherKind string
	otherID   string
	createdAt time.Time
	sourceSeq *int64
	// atBlock is the node id of the queried document's, comment's, message's or ask's block at
	// the queried end — a part, for an ask — on such a read of an edge at one of its blocks.
	atBlock *string
}

// itemKinds is every item kind whose blocks the graph reads, in the order a predicate over all
// three writes them.
var itemKinds = []ItemKind{ItemComment, ItemMessage, ItemAsk}

// itemBlockKinds is the SQL list of the storage kind of every item kind's blocks
// (ItemKind.BlockKind), and blockNodeKinds that list with a document's block kind too, the kinds
// a block node of any kind is stored under. A predicate over them is written from these, so an
// item kind a later build adds reaches every one of them at once.
var (
	itemBlockKinds = blockKindList()
	blockNodeKinds = blockKindList("block")
)

// blockKindList is the parenthesised SQL list of kinds, each item kind's block kind after the
// ones it is given.
func blockKindList(kinds ...string) string {
	quoted := make([]string, 0, len(kinds)+len(itemKinds))
	for _, kind := range kinds {
		quoted = append(quoted, `'`+kind+`'`)
	}
	for _, item := range itemKinds {
		quoted = append(quoted, `'`+item.BlockKind()+`'`)
	}
	return `(` + strings.Join(quoted, ", ") + `)`
}

// heldByABlock is true of a document's, comment's, message's or ask's mention edge (graph_edges
// row alias) when a block of that item holds a link of any kind to the same target, listed or
// not: that block's own edge is listed and counted in its place, so each link shows once. A
// typed block's pointer is also written in the document's text, so the document's mention of the
// target is hidden behind the pointer's edge, and stays hidden while the pointer's target has a
// type the pointer does not allow (block_ref_listed). An item the block index has not reached
// holds no block link, so its own edge stays; an ask's question part holds the links its row
// holds, so the row's edge is the one hidden.
func heldByABlock(alias string) string {
	holders := []string{blockHolds(alias, "artifact", "block_refs", "artifact_id")}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		holders = append(holders, blockHolds(alias, string(item), tables.refs, tables.item))
	}
	return `(` + alias + `.kind = 'mentions' and (` + strings.Join(holders, " or ") + `))`
}

// blockHolds is true of a graph_edges row alias written by an item of fromKind when one of the
// item's blocks holds a link of any kind to its target in table, whose column keys the item.
func blockHolds(alias, fromKind, table, column string) string {
	return `(` + alias + `.from_kind = '` + fromKind + `' and exists (
		select 1 from ` + table + ` b where b.to_kind = ` + alias + `.to_kind and b.to_id = ` + alias + `.to_id
		and b.` + column + `::text = ` + alias + `.from_id))`
}

// blockRefListed is whether the block_refs row alias is listed: the types test of a typed
// block's pointer, made the way the arm of graph_edges that answers the row makes it, so a read
// of the rows themselves and a read of the view cannot answer one link differently. target is
// the node id of the block the link reaches: the row's own to_id, or, for a read that follows a
// target no row holds through its moved_to tuples (refs.BlockLinks), the block it reaches there.
//
// A row that names its node is the tuple arm's (migration 20261109215947): a mention is listed
// whatever its target's type, and a pointer only while block_ref_listed holds of the targets the
// attribute node declares now (block_pointer_targets), so a change of an attribute's targets
// reaches every link of that attribute at its next read, with no pass over documents nobody
// edits. A row that names no node, which a build before the store wrote, is the old block_refs
// arm's, whose test reads the to_types the index copied into the row: such a row follows a
// change of the targets only once a version is indexed again. Beyond a mention it tests no row's
// kind, as the view does not, so a decision's decided_by and from_ask, whose attributes declare no
// targets, are listed as graph_edges lists them; the references read admits only mentions and the
// pointer kinds the schema declares (blockRefLinked).
func blockRefListed(alias, target string) string {
	return `(case when ` + alias + `.node_kind is null
		then ` + alias + `.to_types is null or block_ref_listed(` + alias + `.to_types, ` + target + `)
		else ` + alias + `.kind = 'mentions' or block_ref_listed(block_pointer_targets(` + alias + `.from_node, ` + alias + `.kind), ` + target + `) end)`
}

// documentBlockIDs is the node id of every current block of the document whose ref_key is the
// numbered parameter param: the set an "in" read of the document reads once, and then reads the
// links into each of its blocks by, one id at a time (keyedBlockArm). A deleted block has no row,
// so the edges at it drop out of every list until a version holds its id again.
func documentBlockIDs(param string) string {
	return `(select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = ` + param + `)`
}

// itemBlockIDs is the node id of every current block of the item of kind whose uuid is the
// numbered parameter param: the set an "in" read of the item reads once, and then reads the links
// into each of its blocks by, one id at a time (keyedBlockArm), and the set the read's replies
// rule and answers fold test a reply's block against. A removed block keeps its row with no
// position, and the edges at it drop out of every list until a revision holds its id again.
func itemBlockIDs(kind ItemKind, param string) string {
	tables, _ := kind.tables()
	return `(select b.node_id from ` + tables.blocks + ` b where b.` + tables.item + ` = ` + param + `::uuid and ` + itemBlockCurrent + `)`
}

// itemBlockLinks and itemBlockCurrent are the rule that a link a block of an item holds stands
// while that block is one of the item's current ones. itemBlockLinks is the from of a read of
// the links the blocks of an item of kind hold, its link table aliased r joined to its blocks
// table aliased b by the item and the block id, and itemBlockCurrent the condition on b. A
// removed block keeps its row with no position, so the links it held drop out until a revision
// holds the block again. The words are graph_edges' own for its three arms over these tables; the
// caller selects its columns and writes its own conditions beside itemBlockCurrent. An ask's own
// refs row holds the links of its question alone, so a link written in an option or in the answer
// is in ask_block_refs and in no edge of the ask's row, where a comment's or a message's row holds
// every link its blocks hold.
func itemBlockLinks(kind ItemKind) string {
	tables, _ := kind.tables()
	return tables.refs + ` r join ` + tables.blocks + ` b on b.` + tables.item + ` = r.` + tables.item + ` and b.block_id = r.block_id`
}

// itemBlockCurrent is the condition that the item's block aliased b is one of its item's current
// ones: itemBlockLinks' reader writes it on the block holding a link, and itemBlockIDs on the
// blocks it selects.
const itemBlockCurrent = `b.position is not null`

// keyedBlockArm is the arm of an "in" read over the current blocks of the node read, of storage
// kind blockKind, whose node ids ids selects (documentBlockIDs, itemBlockIDs): each edge that
// ends at one of those blocks, at that block, narrowed by source, the read's filter of a source
// written over the arm's rows aliased g. It reads the ids once, and then, for each id, every arm
// of graph_edges that can end at a block of that kind through its own index of targets, keyed by
// to_kind and to_id (a reply by replies_to_block), each with the view's own rule for a current
// block (migration 20261105014808): a block's pointer while block_ref_listed lists it, a link an
// item's block holds while that block is current (itemBlockLinks), and a reply to an ask's part
// told from one to a comment's block by its prefix. Each projection table the view reads is read
// twice by the row's target, as the view answers it: its rows the store does not answer, by the
// marker the view's arm over the table tests (rowNamesNoNode, a refs row's tuples_stored false,
// repliesNotStored), and its rows the store does answer, through the newest tuple each stands for
// (keyedTupleArm, keyedCitationArm, keyedItemBlockTupleArm, keyedReplyTupleArm), which are the
// view's store arms that can end at a block. Each edge comes once: a node's block ids are
// distinct, document_blocks being keyed by document and block and an item's blocks table holding
// one row for each item and block id.
func keyedBlockArm(blockKind, ids, source string) string {
	arms := []string{
		`select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
				from refs r where r.to_kind = '` + blockKind + `' and r.to_id = k.node_id and not r.tuples_stored`,
		`select 'block', b.from_node, b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq
				from block_refs b where b.to_kind = '` + blockKind + `' and b.to_id = k.node_id
				and (b.to_types is null or block_ref_listed(b.to_types, b.to_id)) and ` + rowNamesNoNode(`b.node_kind`),
		keyedTupleArm(blockKind),
		keyedCitationArm(blockKind),
	}
	for _, item := range itemKinds {
		arms = append(arms, `select '`+item.BlockKind()+`', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
				from `+itemBlockLinks(item)+`
				where r.to_kind = '`+blockKind+`' and r.to_id = k.node_id and `+itemBlockCurrent+` and `+rowNamesNoNode(`r.node_kind`),
			keyedItemBlockTupleArm(item, blockKind))
	}
	// A reply names a block of an item, never a document's block.
	for _, item := range itemKinds {
		if item.BlockKind() == blockKind {
			arms = append(arms, blockRepliesArm(item), keyedReplyTupleArm(item))
		}
	}
	return `select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from ` + ids + ` k (node_id)
			cross join lateral (
				` + strings.Join(arms, `
				union all
				`) + `
			) g (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq)
			where ` + source
}

// newestTuple is the lateral that reads, aliased newest, the newest tuple of the one relation a
// projection row stands for, (fromKind, fromID, kind, toKind, toID), each an expression over that
// row: its at, its legacy_source_seq and whether it asserts (graphstore.CurrentTupleRead, the
// reading currentTuple makes). A keyed tuple arm keeps its row while newest.asserted holds and
// answers the edge at that tuple's time and sequence, as the view's store arms answer it.
func newestTuple(fromKind, fromID, kind, toKind, toID string) string {
	return `cross join lateral (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`,
		`t.at, t.legacy_source_seq, t.asserted`,
		`t.from_kind = `+fromKind+` and t.from_id = `+fromID+` and t.kind = `+kind+`
					and t.to_kind = `+toKind+` and t.to_id = `+toID+` and t.to_kind <> '`+graphstore.KindValue+`'`) + `
				) newest`
}

// storeNodeStands is the view's test of the node a store arm's edge comes from: the store holds
// it and its state is not gone, as the view's join to graph_nodes keeps it.
func storeNodeStands(kind, id string) string {
	return `exists (select 1 from graph_nodes live
					where live.kind = ` + kind + ` and live.id = ` + id + ` and live.state <> '` + graphstore.StateGone + `')`
}

// keyedTupleArm is graph_edges' tuple arm (migration 20261125163554, restated by 20261126045005)
// for the edges that end at
// the block k.node_id, of storage kind blockKind: the links of the block_refs rows that name their
// node, read by the row's target through block_refs_to_source as the arm beside it reads the rows
// that name none. Such a row is the projection of the tuple of the node it names (node_kind and
// node_address), a block's link or an answered ask's shaped, and while the row stands the view
// answers the edge from that node, as the tuple states it: while the newest tuple of its relation
// asserts (newestTuple), while the node is not gone (storeNodeStands), and, for a pointer, while
// block_ref_listed lists its target among the targets its attribute declares now. Its time and its
// sequence are that tuple's, as the view answers them. The source's visibility is the read's
// filter of a source, as for every other arm.
func keyedTupleArm(blockKind string) string {
	return `select b.node_kind, b.node_address, b.kind, b.to_kind, b.to_id, newest.at, newest.legacy_source_seq
				from block_refs b
				` + newestTuple(`b.node_kind`, `b.node_address`, `b.kind`, `b.to_kind`, `b.to_id`) + `
				where b.to_kind = '` + blockKind + `' and b.to_id = k.node_id and b.node_kind is not null and newest.asserted
				and (b.kind = 'mentions' or block_ref_listed(block_pointer_targets(b.node_address, b.kind), b.to_id))
				and ` + storeNodeStands(`b.node_kind`, `b.node_address`)
}

// keyedCitationArm is the store's own arm of graph_edges (its citation, migration 20261126045005)
// for an item's mention that ends at the block k.node_id: the refs rows of an ask, a comment or a
// message that hold tuples_stored, read by target through refs_to as the refs arm beside it reads
// the rows that hold it false. Such a row is the projection of the mention its item's node states
// (citationCarries), and the view answers the edge from that node while the newest tuple of the
// relation asserts (newestTuple) and the node is not gone (storeNodeStands), at that tuple's time
// and sequence. A row of any other source or kind that holds tuples_stored is no edge of the view,
// and none of this arm.
func keyedCitationArm(blockKind string) string {
	sources := make([]string, len(itemKinds))
	for index, item := range itemKinds {
		sources[index] = `'` + string(item) + `'`
	}
	return `select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, newest.at, newest.legacy_source_seq
				from refs r
				` + newestTuple(`r.from_kind`, `r.from_id`, `r.kind`, `r.to_kind`, `r.to_id`) + `
				where r.to_kind = '` + blockKind + `' and r.to_id = k.node_id and r.tuples_stored
				and r.from_kind in (` + strings.Join(sources, ", ") + `) and r.kind = '` + graphstore.TupleMentions + `' and newest.asserted
				and ` + storeNodeStands(`r.from_kind`, `r.from_id`)
}

// keyedItemBlockTupleArm is the view's store arm of every item's blocks (migration
// 20261126045003, restated by 20261126045005) for the links a block of an item of kind holds
// that end at the block k.node_id: the rows of the kind's link table that name their block's
// node, read by target as the arm over the rows that name none reads them, while the block
// holding the link stands at a position (itemBlockLinks, itemBlockCurrent). Such a row is the
// projection of the link its block's node states (itemLinkCarries), whose address the item kind's
// prefix names, and the view answers the edge from that block, under the item's block kind, while
// the newest tuple of the relation asserts (newestTuple) and the node is not gone
// (storeNodeStands), at that tuple's time and sequence.
func keyedItemBlockTupleArm(item ItemKind, blockKind string) string {
	return `select '` + item.BlockKind() + `', r.node_address, r.kind, r.to_kind, r.to_id, newest.at, newest.legacy_source_seq
				from ` + itemBlockLinks(item) + `
				` + newestTuple(`r.node_kind`, `r.node_address`, `r.kind`, `r.to_kind`, `r.to_id`) + `
				where r.to_kind = '` + blockKind + `' and r.to_id = k.node_id and ` + itemBlockCurrent + `
				and r.node_kind = '` + BlockNodeKind + `' and r.node_address like '` + string(item) + `/%' and newest.asserted
				and ` + storeNodeStands(`r.node_kind`, `r.node_address`)
}

// blockRepliesArm is graph_edges' arm of the replies to a block of an item of kind owner, keyed
// by the block's node id k.node_id through the replies' partial index on replies_to_block
// (comments_replies_to_block, messages_replies_to_block), over the replies whose pointers the
// store does not hold (repliesNotStored). A comment answers a comment's block and an ask's part,
// both named in comments.replies_to_block, and the node id's prefix tells the two apart, as the
// view's two arms over comments tell them (repliesPrefix); a message answers a message's block.
func blockRepliesArm(owner ItemKind) string {
	replies := repliesFrom(owner)
	tables, _ := replies.tables()
	return `select '` + string(replies) + `', x.id::text, '` + graphstore.TupleRepliesTo + `', '` + owner.BlockKind() + `', x.replies_to_block, x.created_at, null::bigint
				from ` + tables.items + ` x where x.replies_to_block = k.node_id` + repliesPrefix(owner) + ` and ` + repliesNotStored(replies, "x")
}

// keyedReplyTupleArm is the view's store arm of the replies to a block of an item of kind owner
// (migrations 20261126045001 and 20261126045002, restated by 20261126045005): the replies whose
// pointers the store holds (repliesStored), read by the block they name through the replies'
// partial index on replies_to_block as blockRepliesArm reads the rest. Such a row is the
// projection of the replies_to tuple its node states to the block's node, of kind block, which
// the view renders under owner's block kind, and the view answers the edge while the newest tuple
// of the relation asserts (newestTuple) and the node is not gone (storeNodeStands), at that
// tuple's time and sequence.
func keyedReplyTupleArm(owner ItemKind) string {
	replies := repliesFrom(owner)
	tables, _ := replies.tables()
	return `select '` + string(replies) + `', x.node_address, '` + graphstore.TupleRepliesTo + `', '` + owner.BlockKind() + `', x.replies_to_block, newest.at, newest.legacy_source_seq
				from ` + tables.items + ` x
				` + newestTuple(`x.node_kind`, `x.node_address`, `'`+graphstore.TupleRepliesTo+`'`, `'`+BlockNodeKind+`'`, `x.replies_to_block`) + `
				where x.replies_to_block = k.node_id` + repliesPrefix(owner) + ` and ` + repliesStored(replies, "x") + ` and newest.asserted
				and ` + storeNodeStands(`x.node_kind`, `x.node_address`)
}

// repliesPrefix is the test of the block a reply names that tells a reply to a block of an item
// of kind owner from a reply to a block of another kind the same column names: an ask's part
// starts ask/, and a comment's block does not; a message's replies name a message's block alone.
func repliesPrefix(owner ItemKind) string {
	switch {
	case owner == ItemAsk:
		return ` and x.replies_to_block like 'ask/%'`
	case repliesFrom(owner) == ItemComment:
		return ` and x.replies_to_block not like 'ask/%'`
	}
	return ""
}

// repliesNotStored and repliesStored are the markers the view's arms over the replies of kind
// replies test, on the row aliased alias: a comment's pointers are the store's while the row holds
// tuples_stored (pointersNotStored), and the store's arm reads them from the comment's node; a
// message's replies are the store's from the moment its row names its node (rowNamesNoNode).
func repliesNotStored(replies ItemKind, alias string) string {
	if replies == ItemComment {
		return pointersNotStored(alias)
	}
	return rowNamesNoNode(alias + `.node_kind`)
}

func repliesStored(replies ItemKind, alias string) string {
	stored := alias + `.node_kind = '` + string(replies) + `'`
	if replies == ItemComment {
		stored += ` and ` + alias + `.tuples_stored`
	}
	return stored
}

// repliesToABlockOf is true of a graph_edges row alias that is the replies_to edge of a reply of
// kind replies naming a current block of the item of kind owner whose uuid is the numbered
// parameter param: the reply is listed under that block, by its replies_to edge to it. The two
// kinds are the same for a comment's blocks and a message's; an ask's thread is comments, so a
// reply to one of its parts is a comment.
func repliesToABlockOf(replies, owner ItemKind, alias, param string) string {
	tables, _ := replies.tables()
	return `(` + alias + `.kind = 'replies_to' and ` + alias + `.from_kind = '` + string(replies) + `' and ` + alias + `.from_id in (
		select r.id::text from ` + tables.items + ` r where r.replies_to_block in ` + itemBlockIDs(owner, param) + `))`
}

// repliesFrom is the kind of item that replies to a block of an item of kind: a comment answers a
// comment's block and an ask's part, both in comments.replies_to_block, and a message answers a
// message's block.
func repliesFrom(kind ItemKind) ItemKind {
	if kind == ItemMessage {
		return ItemMessage
	}
	return ItemComment
}

// itemRead is the item whose blocks' edges a query's read lists with its own: an ask, a comment
// or a message, named by its uuid as the item's own edges name it (canonical, lowercase), as
// every item that holds blocks is. An ask, comment or message reference whose id is no such uuid
// names no item with blocks.
func itemRead(query Query) (ItemKind, bool) {
	kind := ItemKind(query.Kind)
	if !slices.Contains(itemKinds, kind) {
		return "", false
	}
	if id, err := uuid.Parse(query.ID); err != nil || id.String() != query.ID {
		return "", false
	}
	return kind, true
}

// blockKindAt is the storage kind of the blocks a read of a node of kind lists edges at: a
// document's blocks, or an ask's, a comment's or a message's.
func blockKindAt(kind string) string {
	if item := ItemKind(kind); slices.Contains(itemKinds, item) {
		return item.BlockKind()
	}
	return "block"
}

// Edges reads a node's edges from graph_edges, newest first, with the other end of each
// resolved to a node and an excerpt of its text. An edge whose other end no longer exists is
// omitted, and so is one whose other end is a node kind this build does not know; on a
// document's read, so is one at a block that no longer exists. An edge whose other end is a
// block carries that block's stored excerpt. Document mentions carry the source's name as
// excerpt; the API replaces it with the containing block.
//
// who is the person or session the read acts for, which decides which nodes it answers at all:
// an edge whose other end is reserved for its author is absent for every other reader, and so
// is one at a block that is (visibility.go).
func Edges(ctx context.Context, q Queryer, who model.Actor, query Query) ([]model.GraphEdge, error) {
	resolved, err := resolveEdges(ctx, q, who, query)
	if err != nil {
		return nil, err
	}
	edges := make([]model.GraphEdge, len(resolved))
	for index, edge := range resolved {
		edges[index] = edge.GraphEdge
	}
	return edges, nil
}

func resolveEdges(ctx context.Context, q Queryer, who model.Actor, query Query) ([]resolvedEdge, error) {
	rows, err := readEdgeRows(ctx, q, who, query)
	if err != nil {
		return nil, err
	}
	lookups := rows
	atKind := blockKindAt(query.Kind)
	for _, row := range rows {
		if row.atBlock != nil {
			lookups = append(lookups, edgeRow{otherKind: atKind, otherID: *row.atBlock})
		}
	}
	nodes, err := resolveNodes(ctx, q, who, query.Direction == "out", lookups)
	if err != nil {
		return nil, err
	}
	edges := make([]resolvedEdge, 0, len(rows))
	for _, row := range rows {
		node, found := nodes[[2]string{row.otherKind, row.otherID}]
		if !found {
			continue
		}
		edge := resolvedEdge{
			GraphEdge: model.GraphEdge{
				Kind:      row.kind,
				Direction: query.Direction,
				Node:      node.GraphNode,
				CreatedAt: row.createdAt,
				SourceSeq: row.sourceSeq,
			},
			refKey: node.refKey,
		}
		if row.atBlock != nil {
			block, found := nodes[[2]string{atKind, *row.atBlock}]
			if !found {
				continue
			}
			edge.Block = &block.GraphNode
		}
		switch {
		case node.BlockID != "":
			// A block's stored excerpt is already cut by BlockExcerpt, the excerpt a document's
			// mention shows.
			edge.Excerpt = &model.GraphExcerpt{BlockID: node.BlockID, Text: node.name(query.ServerURL)}
		case node.text != "":
			edge.Excerpt = &model.GraphExcerpt{Text: node.name(query.ServerURL)}
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

// readEdgeRows reads the rows a query lists, as arms over graph_edges that its filters then
// narrow. A document's, comment's, message's or ask's read adds the edges at its blocks — an
// ask's parts among them (see Query) — and every inbound read lists an item's mention from the
// block holding it (heldByABlock). A whole-document read lists each citer by its item instead,
// an ask citing the document from a part included (asksCitingTheDocument).
//
// An inbound read's arm over the node's blocks reads the node's current block ids once, and then
// each link table by its index of targets for each id (keyedBlockArm), never graph_edges with
// to_id in those ids: the planner does not push that set into the view's arms, so that form read
// refs by to_kind alone and hash-joined every block of that kind before a semi-join kept the
// node's own. Measured at --scale 1 with jit off, it took 165 of the 166 ms of the statement for
// the seed's root message with the most replies and 205 of the 261 ms of the one for the seed's
// anchored document, and it grew with every block link of that kind in the database. The arm over
// the node itself reads graph_edges by to_kind and to_id, which reach each arm's index.
//
// Every arm carries the visibility predicate of the end it answers, bound from the registry
// (visibility.go) and never applied to the rows in Go: an edge whose far end who may not see is
// no row of this read, and neither is one at a block who may not see. That is what keeps a
// count and the list behind it saying the same thing, since the count binds the same predicate
// over the same ends (BacklinkCounts).
//
// The node's own kind is written into the statement as a literal and only its id is bound, as
// the count writes the kind it counts for (backlinkCountSQL). graph_edges is a union of
// arms, most of which fix their from_kind and to_kind as literals of their own, and a kind the
// statement states lets the planner throw every arm that cannot match away before it plans it.
// With the kind bound instead, it can only do that while it plans for the values in hand, so
// Postgres finds its generic plan far dearer than the custom one, keeps making a custom plan
// for every execution, and the read pays for planning the whole view again every time:
// measured on this pod over the chain of 200 blocks, 6.9 ms a step of which 6.5 ms was that
// planning, against 0.4 ms a step once the statement is one plan the connection keeps.
func readEdgeRows(ctx context.Context, q Queryer, who model.Actor, query Query) ([]edgeRow, error) {
	if !slices.Contains(nodeKinds, query.Kind) {
		// A kind no loader resolves is the address of no node this build knows, and the
		// arms answer nothing for it, which is what the read answered while the kind was
		// bound: every arm of the view states a kind of its own. The route reaches this
		// read for a node it has already resolved (resolveNodes drops such an end), so this
		// is what keeps a kind out of the statement rather than a case a caller meets.
		return []edgeRow{}, nil
	}
	kind := `'` + query.Kind + `'`
	args := []any{query.ID}
	// The node's id, and the id again as the uuid an item's or a document's blocks are keyed
	// by, for the reads that need it.
	id, blocks := "$1", "$2"
	document := query.Kind == "artifact"
	item, itemBlocks := itemRead(query)
	if itemBlocks || (query.Direction == "out" && document) {
		args = append(args, query.ID)
	}
	// The acting credential, bound once and named by every predicate this read renders.
	seen := "$" + strconv.Itoa(len(args)+1)
	args = append(args, VisibilityArg(who))
	var arms string
	switch {
	case query.Direction == "in":
		source := `not ` + heldByABlock("g")
		if document && query.WholeDocuments {
			// The document's citers, each by its own edges: a document's, an ask's, a comment's
			// or a message's blocks are listed by their item.
			source = `g.from_kind not in ` + blockNodeKinds
		}
		// The citer is the far end of an inbound edge, and the from side of an edge names an
		// artifact by its uuid.
		source += ` and ` + VisibleEnd("g.from_kind", "g.from_id", false, seen)
		own := source
		if itemBlocks {
			own += ` and not ` + repliesToABlockOf(repliesFrom(item), item, "g", blocks)
		}
		arms = `select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = ` + kind + ` and g.to_id = ` + id + ` and ` + own
		switch {
		case document:
			arms += `
			union all
			` + keyedBlockArm("block", documentBlockIDs(id), VisibleKind("block", "g.to_id", seen)+` and `+source)
			if query.WholeDocuments {
				// An ask's row holds its question's links alone, so the only edge from an ask
				// that cites the document in an option or in its answer is that part's.
				arms += `
			union all
			` + asksCitingTheDocument(id, seen)
			}
		case itemBlocks:
			arms += `
			union all
			` + keyedBlockArm(item.BlockKind(), itemBlockIDs(item, blocks), VisibleKind(item.BlockKind(), "g.to_id", seen)+` and `+source)
		}
	case query.Direction == "out" && document:
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id + ` and not ` + heldByABlock("g") + `
			and ` + VisibleEnd("g.to_kind", "g.to_id", true, seen) + `
			union all
			select b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq, b.from_node
			from block_refs b where b.artifact_id = ` + blocks + `::uuid and ` + blockRefListed("b", "b.to_id") + `
			and ` + VisibleKind("block", "b.from_node", seen) + ` and ` + VisibleEnd("b.to_kind", "b.to_id", true, seen)
	case query.Direction == "out" && itemBlocks:
		tables, _ := item.tables()
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id + ` and not ` + heldByABlock("g") + `
			and ` + VisibleEnd("g.to_kind", "g.to_id", true, seen) + `
			union all
			select r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq, r.from_node
			from ` + itemBlockLinks(item) + `
			where r.` + tables.item + ` = ` + blocks + `::uuid and ` + itemBlockCurrent + `
			and ` + VisibleKind(item.BlockKind(), "r.from_node", seen) + ` and ` + VisibleEnd("r.to_kind", "r.to_id", true, seen)
	case query.Direction == "out":
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id + `
			and ` + VisibleEnd("g.to_kind", "g.to_id", true, seen)
	default:
		return nil, fmt.Errorf("reference direction %q must be in or out", query.Direction)
	}
	var sql strings.Builder
	sql.WriteString(`select kind, other_kind, other_id, created_at, source_seq, at_block from (` + arms + `)
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true`)
	if len(query.Kinds) > 0 {
		args = append(args, query.Kinds)
		sql.WriteString(" and kind = any($" + strconv.Itoa(len(args)) + ")")
	}
	if len(query.ExcludeKinds) > 0 {
		args = append(args, query.ExcludeKinds)
		sql.WriteString(" and not (kind = any($" + strconv.Itoa(len(args)) + "))")
	}
	if foldsAnswers(query) {
		if query.Direction == "in" {
			sql.WriteString(" and not (kind = 'answers' and exists (select 1 from comments c where c.id::text = other_id and (c.reply_to::text = " + id)
			if itemBlocks {
				sql.WriteString(" or c.replies_to_block in " + itemBlockIDs(ItemComment, blocks))
			}
			sql.WriteString(")))")
		} else {
			sql.WriteString(` and not (kind = 'answers' and exists (
				select 1 from comments c where c.id::text = ` + id + ` and (c.reply_to::text = other_id or exists (
					select 1 from comment_blocks b where b.node_id = c.replies_to_block
					and b.comment_id::text = other_id and b.position is not null))))`)
		}
	}
	if query.Since != nil {
		args = append(args, *query.Since)
		sql.WriteString(" and source_seq > $" + strconv.Itoa(len(args)))
	}
	if query.Through != nil {
		args = append(args, *query.Through)
		sql.WriteString(" and source_seq <= $" + strconv.Itoa(len(args)))
	}
	sql.WriteString(" order by created_at desc, kind, other_kind, other_id, at_block")
	rows, err := q.Query(ctx, sql.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read graph edges: %w", err)
	}
	defer rows.Close()
	edges := []edgeRow{}
	for rows.Next() {
		var row edgeRow
		if err := rows.Scan(&row.kind, &row.otherKind, &row.otherID, &row.createdAt, &row.sourceSeq, &row.atBlock); err != nil {
			return nil, fmt.Errorf("scan graph edge: %w", err)
		}
		edges = append(edges, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph edges: %w", err)
	}
	return edges, nil
}

func foldsAnswers(query Query) bool {
	return query.Kind == "comment" &&
		(len(query.Kinds) == 0 || slices.Contains(query.Kinds, "replies_to")) &&
		!slices.Contains(query.ExcludeKinds, "replies_to")
}

// Node resolves the node a query addresses, for the person or session who the read acts for.
// Artifacts are addressed by ref_key on the to side of an edge and by uuid on the from side;
// found is false when no such node exists, and false too for a node reserved for an author who
// is not who - a read of such an address answers, byte for byte, what an address nobody holds
// answers (visibility.go).
func Node(ctx context.Context, q Queryer, who model.Actor, kind, id string, byRefKey bool) (model.GraphNode, bool, error) {
	nodes, err := resolveNodes(ctx, q, who, byRefKey, []edgeRow{{otherKind: kind, otherID: id}})
	if err != nil {
		return model.GraphNode{}, false, err
	}
	node, found := nodes[[2]string{kind, id}]
	return node.GraphNode, found, nil
}

// TargetName is what TargetNames answers for one target: Name, the words it is named by, and
// the marks of a comment or a message its author corrected, which a target naming the post or a
// block of it carries as the post's graph node does (model.GraphNode): EditedAt, the time of its
// author's last edit, and DeletedAt and DeletedBy, the mark of its author's delete. All three are
// nil on a post that stands as first written and on a target of any other kind. Gone is true on
// the one entry for a target nothing addresses now: a block of a deleted post.
type TargetName struct {
	Name      string
	Gone      bool
	EditedAt  *time.Time
	DeletedAt *time.Time
	DeletedBy *model.Actor
}

// TargetNames is the words each of targets is named by where who is shown a reference to it
// rather than its own text: an issue's title, a document's name, an ask's question, a comment's
// or a message's body, a component's title and a block's text, each cut as an edge's excerpt is
// cut (resolvedNode.name), which is what GET /api/v1/references already shows beside an edge at
// that node; serverURL is the deployment whose dashboard URLs are references, so no cut splits
// one. The targets are addressed as the to side of an edge addresses them (TargetOf), so an
// artifact's id is its ref_key. A comment or a message, and a block of one, carries its post's
// marks beside its name (TargetName). A deleted post's node stands, with the empty body its
// delete left, so its name is "" beside its delete mark.
//
// One entry per target that resolves for who: a target with no entry is one nothing addresses
// now — its node was deleted, or its kind is one this build cannot address (resolveNodes) — or
// one who may not see, which each kind's loader leaves out as it leaves out a node that does not
// exist (visibility.go), a target reserved for an author who is not who among them, which answers
// as an address nobody holds does (Node). A caller draws either as a reference whose target is
// gone. A block of a deleted comment or message resolves to nothing too, since the delete indexes
// the empty body, which takes every block out of its post (api's planEditedItem, in
// clearDeletedPostBody), but it has an entry: Gone, with no name and its post's marks, the
// delete's and, where its author edited the post before deleting it, the last edit's time, as a
// block read of it is refused naming the deletion (api's itemBlockNotFound). That entry is read
// for who too, so a block of a post who may not see has none. Each kind is read in one statement
// whatever the number of targets, so a batch costs a statement per kind, not per target; a block
// of a comment or a message that resolves to nothing, whose post targets does not name too, costs
// one statement more per kind of post, which reads whether its post was deleted.
func TargetNames(ctx context.Context, q Queryer, who model.Actor, targets []Target, serverURL string) (map[Target]TargetName, error) {
	rows := make([]edgeRow, len(targets))
	for index, target := range targets {
		rows[index] = edgeRow{otherKind: target.Kind, otherID: target.ID}
	}
	nodes, err := resolveNodes(ctx, q, who, true, rows)
	if err != nil {
		return nil, err
	}
	names := make(map[Target]TargetName, len(nodes))
	for key, node := range nodes {
		names[Target{Kind: key[0], ID: key[1]}] = TargetName{
			Name: node.name(serverURL), EditedAt: node.EditedAt, DeletedAt: node.DeletedAt, DeletedBy: node.DeletedBy,
		}
	}
	if err := deletedPostBlocks(ctx, q, who, targets, names); err != nil {
		return nil, err
	}
	return names, nil
}

// deletedPostBlocks enters into names, Gone and under its post's marks (its delete's and its last
// edit's), each of targets that is a block of a comment or a message (ParseItemBlockNode) with no
// entry in names, whose post its author deleted. A post targets names too has its marks in names
// already; the others are read as nodes of their own kind for who (resolveNodes), in one
// statement per kind of post, and none when every block resolved or every such post is named. A
// post who may not see resolves to nothing, so its blocks get no entry.
func deletedPostBlocks(ctx context.Context, q Queryer, who model.Actor, targets []Target, names map[Target]TargetName) error {
	var posts []edgeRow
	blocks := map[[2]string][]Target{}
	for _, target := range targets {
		if _, named := names[target]; named {
			continue
		}
		kind, itemID, _, ok := ParseItemBlockNode(target.ID)
		if !ok || kind == ItemAsk || target.Kind != kind.BlockKind() {
			continue
		}
		post := [2]string{string(kind), itemID}
		if _, held := blocks[post]; !held {
			if _, named := names[Target{Kind: post[0], ID: post[1]}]; !named {
				posts = append(posts, edgeRow{otherKind: post[0], otherID: post[1]})
			}
		}
		blocks[post] = append(blocks[post], target)
	}
	if len(blocks) == 0 {
		return nil
	}
	nodes, err := resolveNodes(ctx, q, who, true, posts)
	if err != nil {
		return err
	}
	for post, held := range blocks {
		marks, named := names[Target{Kind: post[0], ID: post[1]}]
		if !named {
			node, found := nodes[post]
			if !found {
				continue
			}
			marks = TargetName{EditedAt: node.EditedAt, DeletedAt: node.DeletedAt, DeletedBy: node.DeletedBy}
		}
		if marks.DeletedAt == nil {
			continue
		}
		gone := TargetName{Gone: true, EditedAt: marks.EditedAt, DeletedAt: marks.DeletedAt, DeletedBy: marks.DeletedBy}
		for _, target := range held {
			names[target] = gone
		}
	}
	return nil
}

// resolvedNode is a graph node with the text an excerpt is cut from: a block's is its stored
// excerpt, already cut.
type resolvedNode struct {
	model.GraphNode
	text   string
	refKey string
}

// name is the words shown for the node in place of its text: its own text's first excerptRunes
// runes, cut where no reference crosses the cut (itemExcerpt; serverURL is the deployment whose
// dashboard URLs are references). A block's text is its stored excerpt, already cut by
// BlockExcerpt.
func (n resolvedNode) name(serverURL string) string {
	if n.BlockID != "" {
		return n.text
	}
	return itemExcerpt(n.text, serverURL)
}

// resolveNodes loads every distinct other end by kind, each loader answering only what who may
// see.
func resolveNodes(ctx context.Context, q Queryer, who model.Actor, byRefKey bool, rows []edgeRow) (map[[2]string]resolvedNode, error) {
	return resolveNodesWith(ctx, q, who, byRefKey, rows, nodeRegistry)
}

// resolveNodesWith is resolveNodes resolving each kind through registry in place of nodeRegistry,
// as a build whose registry is registry would: a test reads an older build's answer through it
// without writing the package's registry under every other test of the process.
func resolveNodesWith(ctx context.Context, q Queryer, who model.Actor, byRefKey bool, rows []edgeRow, registry map[string]nodeLoader) (map[[2]string]resolvedNode, error) {
	byKind := make(map[string][]string)
	seen := make(map[[2]string]struct{})
	for _, row := range rows {
		key := [2]string{row.otherKind, row.otherID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		byKind[row.otherKind] = append(byKind[row.otherKind], row.otherID)
	}
	nodes := make(map[[2]string]resolvedNode, len(seen))
	for kind, ids := range byKind {
		load := registry[kind]
		if load == nil {
			// A kind this build cannot address is a node a newer build added, read while both
			// builds serve one graph_edges: its edges are omitted, as an edge whose other end no
			// longer exists is. A kind the registry resolves no nodes of is omitted the same way.
			continue
		}
		if err := load(ctx, q, who, kind, byRefKey, ids, nodes); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// loadStoreNodes resolves the kinds the node and tuple store holds itself: a person, a session,
// a role, a role's or a session's page, and a delivery attempt (graphstore). Each is read from
// graph_nodes, and one the store has no row for is absent from the answer, as every other loader
// leaves out a node that does not exist: a session Dispatch has recorded nothing of is no node of
// the graph, and the edges at it are omitted until a write records it. None of the five carries a
// dispatch:// address or belongs to a node here: what a session is part of is a tuple, which the
// reads that show one follow for themselves.
func loadStoreNodes(ctx context.Context, q Queryer, who model.Actor, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select id, made_by, made_at from graph_nodes
		where kind = $1 and id = any($2) and state = $3 and `+VisibleRow("graph_nodes", "$4"),
		kind, ids, graphstore.StateLive, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var madeBy []byte
		var madeAt time.Time
		if err := rows.Scan(&id, &madeBy, &madeAt); err != nil {
			return fmt.Errorf("scan %s node: %w", kind, err)
		}
		node := model.GraphNode{Kind: kind, ID: id}
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			return err
		}
		nodes[[2]string{kind, id}] = resolvedNode{GraphNode: node}
	}
	return rows.Err()
}

// loadIssueNodes resolves issue nodes. An issue is made by its creator; its last change is the
// latest event that wrote its own fields (issue.created, issue.updated, issue.closed: a claim, a
// deploy notice or a comment is no change of it), by that event's actor. It belongs to its parent
// issue, which has no versions.
func loadIssueNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, keys []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select i.key, i.project_key, i.title, i.created_by, i.created_at, i.parent_key, change.actor, change.created_at
		from issues i
		left join lateral (
			select e.actor, e.created_at from events e
			where e.issue_key = i.key and e.type = any('{issue.created,issue.updated,issue.closed}')
			order by e.seq desc limit 1
		) change on true
		where i.key = any($1) and `+VisibleKind("issue", "i.key", "$2"), keys, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load issue nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, project, title string
		var createdBy, changedBy []byte
		var createdAt time.Time
		var changedAt *time.Time
		var parentKey *string
		if err := rows.Scan(&key, &project, &title, &createdBy, &createdAt, &parentKey, &changedBy, &changedAt); err != nil {
			return fmt.Errorf("scan issue node: %w", err)
		}
		node := model.GraphNode{Kind: "issue", ID: key, IssueKey: new(key), Project: project, Ref: "dispatch://" + key, Parent: issueParent(parentKey)}
		if err := setMadeAndChanged(&node, createdBy, createdAt, changedBy, changedAt, storedActor); err != nil {
			return err
		}
		nodes[[2]string{"issue", key}] = resolvedNode{GraphNode: node, text: title}
	}
	return rows.Err()
}

// RetiredDecisionsSlug is the slug of an issue's decisions document, which the decisions step
// retires: the address `dispatch://KEY/artifact/<this slug>` of a
// retired one resolves to the issue KEY, as every retired document's address does
// (loadArtifactNodes). It is the one place the server spells that slug: api refuses a new
// document that would take it.
const RetiredDecisionsSlug = "decisions-md"

// DecisionsDocumentOf is the FROM and WHERE of a read of the issue's decisions document while it
// stands, issue the SQL that names the issue's key (a parameter, or a column of an outer query),
// with the document's artifacts row aliased `a`: of the documents decisions_documents() answers
// for the issue, the oldest, by created_at and then id. Every read that names the issue's decisions
// document - the issue's decisions_artifact_id, the Decisions read's `document` and the decisions
// step's restore - selects through it, so for an issue that holds two they name the same one.
// kept for one release: decisions-documents-fn
func DecisionsDocumentOf(issue string) string {
	return `from decisions_documents() d join artifacts a on a.id = d.id
		where d.issue_key = ` + issue + ` order by a.created_at, a.id limit 1`
}

// loadArtifactNodes resolves artifact nodes. An artifact is made by its creator; its last change
// is its latest version, by that version's authors. It belongs to its issue, else to the session
// whose conversation owns it, else (a project document) to no node.
//
// The address of a retired document of an issue (artifacts.retired_at, docs.RetireDocument),
// `dispatch://KEY/artifact/<slug>` read by its ref_key, resolves to the issue KEY, whose node it
// answers in the document's place: the document plays no part in
// its issue any more, and a citation of it stays valid and reaches what took its place, the issue
// and its reads. The document read by its uuid is the document still, so its versions, its blocks
// and its own edges stay readable.
func loadArtifactNodes(ctx context.Context, q Queryer, who model.Actor, _ string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error {
	predicate := `a.id::text = any($1)`
	if byRefKey {
		predicate = `a.ref_key = any($1)`
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.ref_key, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at, latest.authors, latest.created_at, a.retired_at is not null
		from artifacts a
		left join lateral (
			select v.authors, v.created_at from artifact_versions v
			where v.artifact_id = a.id order by v.number desc limit 1
		) latest on true
		where `+predicate+` and `+VisibleKind("artifact", "a.ref_key", "$2"), ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load artifact nodes: %w", err)
	}
	defer rows.Close()
	// retiredIssues is the issue each retired document's address resolves to, by that address.
	var retiredIssues map[string]string
	for rows.Next() {
		var id, refKey, project, slug, name, kind string
		var issueKey, session *string
		var primary, retired bool
		var createdBy, authors []byte
		var createdAt time.Time
		var versionAt *time.Time
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &name, &kind, &primary, &createdBy, &createdAt, &authors, &versionAt, &retired); err != nil {
			return fmt.Errorf("scan artifact node: %w", err)
		}
		lookup := id
		if byRefKey {
			lookup = refKey
		}
		if byRefKey && retired && issueKey != nil {
			if retiredIssues == nil {
				retiredIssues = make(map[string]string)
			}
			retiredIssues[lookup] = *issueKey
			continue
		}
		node := model.GraphNode{Kind: "artifact", ID: id, IssueKey: issueKey, Project: project, Ref: ArtifactRef(issueKey, project, session, slug, kind, primary)}
		if err := setMadeAndChanged(&node, createdBy, createdAt, authors, versionAt, storedActors); err != nil {
			return err
		}
		switch {
		case issueKey != nil:
			node.Parent = issueParent(issueKey)
		case session != nil:
			node.Parent = &model.NodeParent{Kind: "session", ID: *session}
		}
		nodes[[2]string{"artifact", lookup}] = resolvedNode{GraphNode: node, text: name, refKey: refKey}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(retiredIssues) == 0 {
		return nil
	}
	keys := make([]string, 0, len(retiredIssues))
	for _, key := range retiredIssues {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	// The issue is read as the issue loader reads it, for who: an issue they may not see answers
	// what an address nobody holds answers, as its own address would.
	issues := make(map[[2]string]resolvedNode, len(keys))
	if err := loadIssueNodes(ctx, q, who, "issue", false, keys, issues); err != nil {
		return err
	}
	for lookup, key := range retiredIssues {
		if issue, found := issues[[2]string{"issue", key}]; found {
			nodes[[2]string{"artifact", lookup}] = issue
		}
	}
	return nil
}

// loadBlockNodes resolves block nodes, addressed '<document ref_key>#<block id>', from the
// current blocks of their documents' latest indexed versions. A block no current version holds
// has no row and resolves to nothing, so every edge at it is omitted until a version holds its
// id again. A block is made by the authors of the version that first held it, when Dispatch knows
// that version (blockFirstVersion); its last change is its changed version, by that version's
// authors. It belongs to its document at its changed version.
//
// Its joins to artifacts and artifact_versions are lateral probes by key (nodeLoader).
func loadBlockNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	held, args := blockNodesAddressed(ids, who)
	rows, err := q.Query(ctx, `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at, changed.authors,
		       `+documentColumns("a")+`, `+blockFirstColumns("first")+`
		from document_blocks d
		cross join lateral (
			select a.id, a.issue_key, a.project_key, a.session_id, a.slug, a.kind, a.is_primary
			from artifacts a where a.id = d.artifact_id limit 1
		) a
		left join lateral (
			select changed.authors from artifact_versions changed
			where changed.artifact_id = d.artifact_id and changed.number = d.changed_version limit 1
		) changed on true
		`+blockFirstVersion("first", "d.artifact_id", "d.block_id")+`
		where `+held+` and `+VisibleKind("block", "d.node_id", "$2"), args...)
	if err != nil {
		return fmt.Errorf("load block nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, blockID, excerpt string
		var changedVersion int
		var changedAt time.Time
		var changedBy []byte
		var document documentParent
		var first blockFirst
		targets := append([]any{&nodeID, &blockID, &excerpt, &changedVersion, &changedAt, &changedBy}, document.targets()...)
		if err := rows.Scan(append(targets, first.targets()...)...); err != nil {
			return fmt.Errorf("scan block node: %w", err)
		}
		changers, err := storedActors(changedBy)
		if err != nil {
			return fmt.Errorf("decode the last change of block %s: %w", nodeID, err)
		}
		parent := document.parent()
		node := model.GraphNode{
			Kind:           "block",
			ID:             nodeID,
			BlockID:        blockID,
			IssueKey:       document.issueKey,
			Project:        document.project,
			Ref:            BlockRef(parent.Ref, blockID),
			Document:       parent.Ref,
			ChangedVersion: changedVersion,
			ChangedBy:      changers,
			ChangedAt:      &changedAt,
			Parent:         parent,
			ParentVersion:  &changedVersion,
		}
		if node.MadeBy, node.MadeAt, err = first.maker(); err != nil {
			return fmt.Errorf("decode the maker of block %s: %w", nodeID, err)
		}
		nodes[[2]string{"block", nodeID}] = resolvedNode{GraphNode: node, text: excerpt}
	}
	return rows.Err()
}

// blockNodesAddressed is how loadBlockNodes names the blocks it reads, and the arguments that
// go with it: the node ids bound as $1 and the acting credential as $2.
//
// One block is bound as itself. That is what every read of one node asks for - the node a
// `dispatch://...#b-...` address names, and the far end of a chain step's one edge - and a
// single equality on the hash index over document_blocks.node_id is a plan the planner builds
// the same way whether or not it knows the value, so Postgres keeps one plan for the statement
// and the read stops paying to plan it again: measured on this pod over the read-cost corpus,
// 2.5 ms a call against 0.2 ms once the plan is the connection's.
//
// Two or more arrive as an array, under QueryExecModeDescribeExec so that the statement is
// planned with its values. An array tells a generic plan nothing about its length, so that
// plan reads it as an unknown-length scalar-array operation rather than a constant it can
// hash: measured on this pod over a document of 8,001 blocks, 460 ms against 90 ms with the
// values in hand.
func blockNodesAddressed(ids []string, who model.Actor) (string, []any) {
	if len(ids) == 1 {
		return `d.node_id = $1`, []any{ids[0], VisibilityArg(who)}
	}
	return `d.node_id = any($1)`, []any{pgx.QueryExecModeDescribeExec, ids, VisibilityArg(who)}
}

// BlockRef is the reference to block blockID of the document whose reference is document:
// `<document>#b-<text.EncodeBlockID(blockID)>`. A document with no address has none.
func BlockRef(document, blockID string) string {
	if document == "" {
		return ""
	}
	return document + "#b-" + text.EncodeBlockID(blockID)
}

// anchorVersion is the version an item's anchor (alias.anchor) names, null when it names none a
// version number can be: the anchor is written by the server, and the read never fails on it.
func anchorVersion(alias string) string {
	return `case when ` + alias + `.anchor->>'version' ~ '^[1-9][0-9]{0,8}$' then (` + alias + `.anchor->>'version')::int end`
}

// loadAskNodes resolves ask nodes. An ask is made by its author. Its last change is its own edit
// (edited_at: its wording, a block ask's settlement, an approval request's version move), by the
// editor the latest ask.edited event names; an answer or a resolution is no change of it. It
// belongs to the document it is written on: the one its anchor names, when the anchor's
// artifact_id casts to a uuid, at the anchored version; a block ask's document, at the version
// that first held its block when Dispatch knows it (blockFirstVersion); an approval request's
// document, at the version it named when it opened (written_version); the document that owns it,
// at the version current when it was written (written_version). An ask written on no document
// belongs to its issue.
//
// An artifact_id that casts to no uuid names no document, so such an anchor falls through to the
// branch below - the ask's block document, at that branch's version - exactly as an anchor
// carrying no artifact_id at all does, and as both search arms link the same ask. The version and
// the document therefore always come from one branch.
//
// The edit lookup repeats events_ask_payload_id's type list (migration 0030), so the planner can
// prove the partial index serves it.
//
// Its joins to issues and artifacts are lateral probes by key (nodeLoader).
func loadAskNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	anchorArtifact := store.CastableUUID(`k.anchor->>'artifact_id'`)
	rows, err := q.Query(ctx, `
		select k.id::text, k.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), k.question,
		       k.author, k.created_at, k.edited_at, edit.edited_by, `+documentColumns("doc")+`,
		       case
		         when `+anchorArtifact+` is not null then `+anchorVersion("k")+`
		         when k.block_artifact_id is not null then case when first.known then first.version end
		         else k.written_version
		       end
		from asks k
		left join lateral (select i.project_key from issues i where i.key = k.issue_key limit 1) i on true
		left join lateral (select a.project_key, a.slug from artifacts a where a.id = k.artifact_id limit 1) a on true
		left join lateral (
			select doc.id, doc.issue_key, doc.project_key, doc.session_id, doc.slug, doc.kind, doc.is_primary
			from artifacts doc where doc.id = case
			  when `+anchorArtifact+` is not null then `+anchorArtifact+`
			  when k.block_artifact_id is not null then k.block_artifact_id
			  when k.kind = 'approval' then (k.approval->>'artifact_id')::uuid
			  else k.artifact_id
			end
			limit 1
		) doc on true
		left join lateral (
			select e.payload->'edited_by' as edited_by from events e
			where e.type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited') and e.type = 'ask.edited'
			  and e.payload->>'id' = k.id::text
			order by e.id desc limit 1
		) edit on k.edited_at is not null
		`+blockFirstVersion("first", "k.block_artifact_id", "k.block_id")+`
		where k.id::text = any($1) and `+VisibleKind("ask", "k.id::text", "$2"), ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load ask nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, slug, question string
		var issueKey *string
		var author, editedBy []byte
		var createdAt time.Time
		var editedAt *time.Time
		var document documentParent
		var version *int
		targets := append([]any{&id, &issueKey, &project, &slug, &question, &author, &createdAt, &editedAt, &editedBy}, document.targets()...)
		if err := rows.Scan(append(targets, &version)...); err != nil {
			return fmt.Errorf("scan ask node: %w", err)
		}
		node := model.GraphNode{Kind: "ask", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("ask", issueKey, project, slug, id)}
		if err := setMadeAndChanged(&node, author, createdAt, editedBy, editedAt, storedActor); err != nil {
			return err
		}
		node.Parent = issueParent(issueKey)
		if parent := document.parent(); parent != nil {
			node.Parent, node.ParentVersion = parent, version
		}
		nodes[[2]string{"ask", id}] = resolvedNode{GraphNode: node, text: question}
	}
	return rows.Err()
}

// loadCommentNodes resolves comment nodes. A comment is made by its author, and only its author
// edits it, so its last change is its author at edited_at; a resolution is no change of it, and
// a delete is no change of it either: it is the mark model.DeletedPostMark reads, which the node
// carries beside the empty body a delete leaves. A reply that names a block it answers belongs to
// that block, another reply to its thread's head comment, a comment in an ask's thread to the
// ask, and another comment to the document it is written on: the one its anchor names, at the
// anchored version, else the document that owns it, at the version current when it was written
// (written_version). A comment written on no document belongs to its issue. A reply shares its
// head's owner, and so the owner its reference, and the reference of the block it names, are
// written under.
//
// Its joins to issues and artifacts are lateral probes by key (nodeLoader).
func loadCommentNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select c.id::text, c.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), c.body,
		       c.author, c.created_at, c.edited_at, c.deleted_at, c.deleted_by, c.reply_to::text, c.replies_to_block, c.ask_id::text, `+documentColumns("doc")+`,
		       case when c.anchor->>'artifact_id' is not null then `+anchorVersion("c")+` else c.written_version end
		from comments c
		left join lateral (select i.project_key from issues i where i.key = c.issue_key limit 1) i on true
		left join lateral (select a.project_key, a.slug from artifacts a where a.id = c.artifact_id limit 1) a on true
		left join lateral (
			select doc.id, doc.issue_key, doc.project_key, doc.session_id, doc.slug, doc.kind, doc.is_primary
			from artifacts doc where doc.id = coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id) limit 1
		) doc on true
		where c.id::text = any($1) and `+VisibleKind("comment", "c.id::text", "$2"), ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load comment nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, slug, body string
		var issueKey, replyTo, replyBlock, askID *string
		var author, deletedBy []byte
		var createdAt time.Time
		var editedAt, deletedAt *time.Time
		var document documentParent
		var version *int
		targets := append([]any{&id, &issueKey, &project, &slug, &body, &author, &createdAt, &editedAt, &deletedAt, &deletedBy, &replyTo, &replyBlock, &askID}, document.targets()...)
		if err := rows.Scan(append(targets, &version)...); err != nil {
			return fmt.Errorf("scan comment node: %w", err)
		}
		node := model.GraphNode{Kind: "comment", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("comment", issueKey, project, slug, id), EditedAt: editedAt}
		if err := setMadeAndChanged(&node, author, createdAt, author, editedAt, storedActor); err != nil {
			return err
		}
		if node.DeletedAt, node.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
			return fmt.Errorf("comment node %s: %w", id, err)
		}
		switch parent := document.parent(); {
		case replyBlock != nil:
			node.Parent = itemBlockParent(issueKey, project, slug, *replyBlock)
		case replyTo != nil:
			node.Parent = itemParent("comment", issueKey, project, slug, *replyTo)
		case askID != nil:
			node.Parent = itemParent("ask", issueKey, project, slug, *askID)
		case parent != nil:
			node.Parent, node.ParentVersion = parent, version
		default:
			node.Parent = issueParent(issueKey)
		}
		nodes[[2]string{"comment", id}] = resolvedNode{GraphNode: node, text: body}
	}
	return rows.Err()
}

// loadMessageNodes resolves message nodes. A message is made by its author, and only its author
// edits it (PATCH /api/v1/messages/{id}, NOT_AUTHOR), so its last change is its author at
// edited_at, as a comment's is, and a delete is the mark model.DeletedPostMark reads, which the
// node carries beside the empty body a delete leaves. A reply that names a block it answers
// belongs to that block, another reply to the message it answers, which shares its issue, and
// another message to its issue; a direct message belongs to no node.
//
// Its join to issues is a lateral probe by key (nodeLoader).
func loadMessageNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body, m.author, m.created_at, m.edited_at,
		       m.deleted_at, m.deleted_by, m.in_reply_to::text, m.replies_to_block
		from messages m
		left join lateral (select i.project_key from issues i where i.key = m.issue_key limit 1) i on true
		where m.id::text = any($1) and `+VisibleKind("message", "m.id::text", "$2"), ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load message nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, body string
		var issueKey, inReplyTo, replyBlock *string
		var author, deletedBy []byte
		var createdAt time.Time
		var editedAt, deletedAt *time.Time
		if err := rows.Scan(&id, &issueKey, &project, &body, &author, &createdAt, &editedAt, &deletedAt, &deletedBy, &inReplyTo, &replyBlock); err != nil {
			return fmt.Errorf("scan message node: %w", err)
		}
		node := model.GraphNode{Kind: "message", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("message", issueKey, project, "", id), EditedAt: editedAt}
		if err := setMadeAndChanged(&node, author, createdAt, author, editedAt, storedActor); err != nil {
			return err
		}
		if node.DeletedAt, node.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
			return fmt.Errorf("message node %s: %w", id, err)
		}
		switch {
		case replyBlock != nil:
			node.Parent = itemBlockParent(issueKey, project, "", *replyBlock)
		case inReplyTo != nil:
			node.Parent = itemParent("message", issueKey, project, "", *inReplyTo)
		default:
			node.Parent = issueParent(issueKey)
		}
		nodes[[2]string{"message", id}] = resolvedNode{GraphNode: node, text: body}
	}
	return rows.Err()
}

// loadComponentNodes resolves component nodes addressed '<project>/<id>' from the current
// model. A component a re-import retired resolves to nothing, so edges at it are omitted like
// any edge whose other end no longer exists. Dispatch does not import who made or changed a
// component; component_facts records when, and a component with no row there, or one whose
// times were never recorded, has none. A component belongs to its parent component while the
// model holds it.
func loadComponentNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select c.project_key, c.id, c.title, f.made_at, f.changed_at, p.id
		from components c
		left join component_facts f on f.project_key = c.project_key and f.id = c.id
		left join components p on p.project_key = c.project_key and p.id = c.parent
		where c.project_key || '/' || c.id = any($1)
		and `+VisibleKind("component", "c.project_key || '/' || c.id", "$2"), ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load component nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, id, title string
		var madeAt, changedAt *time.Time
		var parent *string
		if err := rows.Scan(&project, &id, &title, &madeAt, &changedAt, &parent); err != nil {
			return fmt.Errorf("scan component node: %w", err)
		}
		address := project + "/" + id
		node := model.GraphNode{
			Kind: "component", ID: address, Project: project, Ref: componentRef(project, id),
			MadeBy: []model.Actor{}, MadeAt: madeAt, ChangedBy: []model.Actor{}, ChangedAt: changedAt,
		}
		if parent != nil {
			node.Parent = &model.NodeParent{Kind: "component", ID: project + "/" + *parent, Ref: componentRef(project, *parent)}
		}
		nodes[[2]string{"component", address}] = resolvedNode{GraphNode: node, text: title}
	}
	return rows.Err()
}

// componentRef is the reference to component id of project.
func componentRef(project, id string) string {
	return "dispatch://" + project + "/component/" + id
}

// issueParent is issue key as a node's parent, or nil for no issue.
func issueParent(key *string) *model.NodeParent {
	if key == nil {
		return nil
	}
	return &model.NodeParent{Kind: "issue", ID: *key, Ref: "dispatch://" + *key}
}

// itemParent is the ask, comment, or message id, addressed as itemRef addresses it, as a node's parent.
func itemParent(kind string, issueKey *string, project, slug, id string) *model.NodeParent {
	return &model.NodeParent{Kind: kind, ID: id, Ref: itemRef(kind, issueKey, project, slug, id)}
}

// documentColumns selects, from the artifacts row aliased alias, what a document parent's node
// id and reference are made of, in documentParent.targets' order.
func documentColumns(alias string) string {
	return alias + `.id::text, ` + alias + `.issue_key, coalesce(` + alias + `.project_key, ''), ` +
		alias + `.session_id, ` + alias + `.slug, ` + alias + `.kind, ` + alias + `.is_primary`
}

// documentParent is the document a node belongs to, as documentColumns selects it; a left join
// that found no document leaves id nil.
type documentParent struct {
	id, issueKey, session, slug, kind *string
	project                           string
	primary                           *bool
}

func (d *documentParent) targets() []any {
	return []any{&d.id, &d.issueKey, &d.project, &d.session, &d.slug, &d.kind, &d.primary}
}

// parent is the document as a node's parent: its uuid and its reference, absent when it has
// none; nil when there is no document.
func (d documentParent) parent() *model.NodeParent {
	if d.id == nil {
		return nil
	}
	return &model.NodeParent{Kind: "artifact", ID: *d.id, Ref: ArtifactRef(d.issueKey, d.project, d.session, *d.slug, *d.kind, *d.primary)}
}

// storedActor decodes a single-actor column (a creator, an author, an event's actor, an edit's
// editor) as a node's actors, a list of that one actor. SQL NULL (a left join that found no row,
// an edit that names no editor) and JSON null are nobody recorded, the empty list; any other value
// that does not decode as a model.Actor fails the read.
func storedActor(raw []byte) ([]model.Actor, error) {
	var actor *model.Actor
	if raw != nil {
		if err := json.Unmarshal(raw, &actor); err != nil {
			return nil, fmt.Errorf("decode actor: %w", err)
		}
	}
	if actor == nil {
		return []model.Actor{}, nil
	}
	return []model.Actor{*actor}, nil
}

// storedActors decodes a list column (a version's authors) as a node's actors. SQL NULL (a left
// join that found no version) and JSON null are nobody recorded, the empty list, as is an empty
// list; any other value that does not decode as a list of model.Actor fails the read.
func storedActors(raw []byte) ([]model.Actor, error) {
	var actors []model.Actor
	if raw != nil {
		if err := json.Unmarshal(raw, &actors); err != nil {
			return nil, fmt.Errorf("decode actors: %w", err)
		}
	}
	if actors == nil {
		return []model.Actor{}, nil
	}
	return actors, nil
}

// setMadeAndChanged sets node's maker, the single-actor column madeBy (storedActor), and its last
// change, which is the maker until changedAt is set and then changedBy as decodeChanged reads it
// (storedActor or storedActors). A column that does not decode fails, naming node's kind and id.
func setMadeAndChanged(node *model.GraphNode, madeBy []byte, madeAt time.Time, changedBy []byte, changedAt *time.Time, decodeChanged func([]byte) ([]model.Actor, error)) error {
	makers, err := storedActor(madeBy)
	if err != nil {
		return fmt.Errorf("decode the maker of %s %s: %w", node.Kind, node.ID, err)
	}
	node.MadeBy, node.MadeAt = makers, &madeAt
	node.ChangedBy, node.ChangedAt = node.MadeBy, node.MadeAt
	if changedAt != nil {
		changers, err := decodeChanged(changedBy)
		if err != nil {
			return fmt.Errorf("decode the last change of %s %s: %w", node.Kind, node.ID, err)
		}
		node.ChangedBy, node.ChangedAt = changers, changedAt
	}
	return nil
}

// ArtifactRef is the address the dashboard copies for a document: an issue's primary document
// is its spec, another issue artifact sits under artifact/<slug>, a project document under
// its project, and an agent's conversation's file under agent/<session id>.
func ArtifactRef(issueKey *string, project string, session *string, slug, kind string, primary bool) string {
	if session != nil {
		return "dispatch://agent/" + *session + "/artifact/" + slug
	}
	if issueKey == nil {
		if project == "" {
			// The row's owner is one a later schema adds (LEGION-541); main names no address for it.
			return ""
		}
		return "dispatch://" + project + "/artifact/" + slug
	}
	if primary && kind == "doc" {
		return "dispatch://" + *issueKey + "/spec"
	}
	return "dispatch://" + *issueKey + "/artifact/" + slug
}

// itemRef addresses an ask, comment, or message: under its issue, or under the project document
// that owns it. An issue-less message has no address, nor has an item on an artifact with no
// project, whose owner is one a later schema adds (LEGION-541).
func itemRef(kind string, issueKey *string, project, slug, id string) string {
	if issueKey != nil {
		return "dispatch://" + *issueKey + "/" + kind + "/" + id
	}
	if slug == "" || project == "" {
		return ""
	}
	return "dispatch://" + project + "/artifact/" + slug + "/" + kind + "/" + id
}
