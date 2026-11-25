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

// excerptRunes bounds the head of an item's text shown beside an edge.
const excerptRunes = 240

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
	listed := slices.Clone(kinds)
	for _, item := range itemKinds {
		listed = append(listed, item.BlockKind())
	}
	return `(` + store.QuoteLiterals(listed) + `)`
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

// edgeFrom is the from end of the graph_edges row alias as the from side names a node everywhere
// else: an artifact by its uuid (Query, resolveNodes). The view's tuple arm names a node as
// graph_nodes keys it, and the store keys a document's node by its ref_key
// (graphstore.ArtifactNode), so the one relation the store states out of a document with no
// projection row behind it - the changed_by_run a write made for a run appends
// (graphstore.TupleChangedByRun), the only kind of the view's store class whose from end may be a
// document - names the document by its ref_key. This reads that one back as the document's uuid,
// so an inbound read checks and resolves its from end as it does every other and lists the edge
// the run's count holds (changedByRun). A ref_key no artifact holds any more is left as it is, and
// the read drops its edge as it drops one at a node that no longer exists.
func edgeFrom(alias string) string {
	return `(case when ` + alias + `.from_kind = 'artifact' and ` + alias + `.kind = '` + graphstore.TupleChangedByRun + `'
		then coalesce((select edge_artifact.id::text from artifacts edge_artifact where edge_artifact.ref_key = ` + alias + `.from_id), ` + alias + `.from_id)
		else ` + alias + `.from_id end)`
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
// links into each of its blocks by, one id at a time (keyedBlockArm), as the ask parts that cite
// them are read (asksCitingTheDocument). A deleted block has no row, so the edges at it drop out
// of every list until a version holds its id again.
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
// told from one to a comment's block by its prefix. The view's arm over block_refs answers only
// the rows that name no node (rowNamesNoNode), and its tuple arm the links of the rows that do
// (keyedTupleArm), so this reads both, each by the row's target, as the view answers them; and the
// view answers the store's own relations into the block from the tuples alone, a lens invoked on
// it and a risk whose check it is, which this reads by the tuple's far end (keyedStoreArm). Each
// edge comes once: a node's block ids are distinct, document_blocks being keyed by document and
// block and an item's blocks table holding one row for each item and block id.
func keyedBlockArm(blockKind, ids, source string) string {
	arms := []string{
		`select r.from_kind, r.from_id, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
				from refs r where r.to_kind = '` + blockKind + `' and r.to_id = k.node_id`,
		`select 'block', b.from_node, b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq
				from block_refs b where b.to_kind = '` + blockKind + `' and b.to_id = k.node_id
				and (b.to_types is null or block_ref_listed(b.to_types, b.to_id)) and ` + rowNamesNoNode(`b.node_kind`),
	}
	for _, item := range itemKinds {
		arms = append(arms, `select '`+item.BlockKind()+`', r.from_node, r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq
				from `+itemBlockLinks(item)+`
				where r.to_kind = '`+blockKind+`' and r.to_id = k.node_id and `+itemBlockCurrent)
	}
	// A reply names a block of an item, never a document's block.
	for _, item := range itemKinds {
		if item.BlockKind() == blockKind {
			arms = append(arms, blockRepliesArm(item))
		}
	}
	arms = append(arms, keyedTupleArm(blockKind), keyedStoreArm(blockKind))
	return `select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from ` + ids + ` k (node_id)
			cross join lateral (
				` + strings.Join(arms, `
				union all
				`) + `
			) g (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq)
			where ` + source
}

// keyedTupleArm is graph_edges' tuple arm (migration 20261121104002, which restates
// 20261121034242's arm with the store's own class beside it, keyedStoreArm) for the edges that
// end at the block k.node_id, of storage kind blockKind: its class block_ref, the links of the
// block_refs rows that name their node, read by the row's target through block_refs_to_source as
// the arm beside it reads the rows that name none. Such a row is the projection of the tuple of
// the node it names (node_kind and node_address), a block's link or an answered ask's shaped, and
// while the row stands the view answers the edge from that node, as the tuple states it: while the
// newest tuple of its relation asserts (graphstore.CurrentTupleRead, the reading currentTuple
// makes), while the node is not gone, as the view's join to graph_nodes keeps it, and, for a
// pointer, while block_ref_listed lists its target among the targets its attribute declares now.
// Its time and its sequence are that tuple's, as the view answers them. The source's visibility is
// the read's filter of a source, as for every other arm.
func keyedTupleArm(blockKind string) string {
	return `select b.node_kind, b.node_address, b.kind, b.to_kind, b.to_id, newest.at, newest.legacy_source_seq
				from block_refs b
				cross join lateral (` + graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id`,
		`t.at, t.legacy_source_seq, t.asserted`,
		`t.from_kind = b.node_kind and t.from_id = b.node_address and t.kind = b.kind
					and t.to_kind = b.to_kind and t.to_id = b.to_id and t.to_kind <> '`+graphstore.KindValue+`'`) + `
				) newest
				where b.to_kind = '` + blockKind + `' and b.to_id = k.node_id and b.node_kind is not null and newest.asserted
				and (b.kind = 'mentions' or block_ref_listed(block_pointer_targets(b.node_address, b.kind), b.to_id))
				and exists (select 1 from graph_nodes live
					where live.kind = b.node_kind and live.id = b.node_address and live.state <> '` + graphstore.StateGone + `')`
}

// keyedStoreArm is graph_edges' two arms of the store alone (migration 20261121104002) for the
// edges that end at the block k.node_id, of storage kind blockKind: its tuple arm's class store,
// the relations the view answers from the tuples with no projection row behind them, and the arm
// beside it that answers the same relations out of a finding of a lens, as lens_block. A lens
// invoked on the block stands on it by its part_of, and a risk whose check the block is points at
// it by its prevented_by, so the "in" read of a document or an item lists both at the block, as
// the view does. Each relation is read by its far end and its kind through graph_tuples_as_of_in:
// the newest tuple of it, kept while that one asserts (graphstore.CurrentTupleRead), out of a node
// that is not gone, as the view's join to graph_nodes keeps it. A finding is a block row of the
// store whose id names its lens, and the view writes its kind as lens_block. The source's
// visibility, and a standing kind's staleness, is the read's filter of a source, as for every
// other arm (VisibleEnd over AnyKind). The kinds it reads are storeArmPointers out of any node,
// and storeArmPlacements and authored_by only out of a node of the store's own kinds or a finding.
func keyedStoreArm(blockKind string) string {
	finding := `t.from_kind = '` + BlockNodeKind + `' and ` + graphstore.FindingIDSQL(`t.from_id`)
	pointers := store.QuoteLiterals(storeArmPointers)
	placements := store.QuoteLiterals(slices.Concat(storeArmPlacements, []string{graphstore.TupleAuthoredBy}))
	sources := store.QuoteLiterals([]string{graphstore.KindInteraction, graphstore.KindLens, graphstore.KindRisk, graphstore.KindConstraint})
	return `select ` + graphstore.AddressKindSQL(`newest.from_kind`, `newest.from_id`) + `,
				       newest.from_id, newest.kind, newest.to_kind, newest.to_id, newest.at, newest.legacy_source_seq
				from (` + graphstore.CurrentTupleRead(`t.kind, t.from_kind, t.from_id`,
		`t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, t.at, t.legacy_source_seq, t.asserted`,
		`t.to_kind = '`+blockKind+`' and t.to_id = k.node_id
					and (t.kind in (`+pointers+`) or t.kind in (`+placements+`) and (t.from_kind in (`+sources+`) or `+finding+`))`) + `
				) newest
				where newest.asserted and exists (select 1 from graph_nodes live
					where live.kind = newest.from_kind and live.id = newest.from_id and live.state <> '` + graphstore.StateGone + `')`
}

// storeArmPointers are the relations keyedStoreArm reads out of a node of any kind: each pointer
// graphstore.PointerKinds lists, read once, and changed_by_run. changed_by_run is listed because
// the view's store class answers it from the tuples alone, out of any node, as it answers each
// pointer (migration 20261121104002), and TupleKinds (backlink_counts.go) reads the store's own
// kinds from this list and storeArmPlacements. It is no pointer: a write made for a run appends a
// changed_by_run out of each node it created or changed to the run's interaction (graphstore's
// changedByRunMarks and queueChangedByRun, in the append QueueWrite and QueueCaused queue), and no
// pointer route moves one, so graphstore's pointerRules holds no rule for it and PointerKinds does
// not list it. Each pointer of pointerRules is here by construction;
// TestTheStoreArmReadsEveryKindThePointerRulesHold holds each of its placements to
// storeArmPlacements, and none of those to PointerKinds.
var storeArmPointers = append([]string{graphstore.TupleChangedByRun}, graphstore.PointerKinds()...)

// storeArmPlacements are the kinds of graphstore's pointerRules that keyedStoreArm leaves out of
// storeArmPointers: part_of, which the table holds as a placement (graphstore.PlacementKinds), a
// risk's place under a pre-mortem lens, not as a pointer, so PointerKinds leaves it out. The arm
// reads it, beside authored_by, only out of a node of the store's own kinds or out of a finding,
// because the view renders no part_of tuple out of any other node: a session's, a page's and a
// block's placement are part_of tuples no arm of it renders (TupleKinds, backlink_counts.go). It
// is a hand list: a placement added to graphstore's pointerRules must join it, and
// TestTheStoreArmReadsEveryKindThePointerRulesHold (store_arm_kinds_test.go) fails until it does.
var storeArmPlacements = []string{graphstore.TuplePartOf}

// blockRepliesArm is graph_edges' arm of the replies to a block of an item of kind owner, keyed
// by the block's node id k.node_id through the replies' partial index on replies_to_block
// (comments_replies_to_block, messages_replies_to_block). A comment answers a comment's block and
// an ask's part, both named in comments.replies_to_block, and the node id's prefix tells the two
// apart, as the view's two arms over comments tell them; a message answers a message's block.
func blockRepliesArm(owner ItemKind) string {
	replies := repliesFrom(owner)
	tables, _ := replies.tables()
	prefix := ""
	switch {
	case owner == ItemAsk:
		prefix = ` and x.replies_to_block like 'ask/%'`
	case replies == ItemComment:
		prefix = ` and x.replies_to_block not like 'ask/%'`
	}
	return `select '` + string(replies) + `', x.id::text, 'replies_to', '` + owner.BlockKind() + `', x.replies_to_block, x.created_at, null::bigint
				from ` + tables.items + ` x where x.replies_to_block = k.node_id` + prefix
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
		edge.Excerpt = excerptOf(node)
		edges = append(edges, edge)
	}
	return edges, nil
}

// excerptOf is the excerpt an edge at node shows: its name (resolvedNode.name), a block's stored
// excerpt with its block id and the head of any other node's own text, and nil for a node that
// has no text.
func excerptOf(node resolvedNode) *model.GraphExcerpt {
	switch {
	case node.BlockID != "":
		// A block's stored excerpt is already cut to BlockExcerptRunes, the excerpt a
		// document's mention shows.
		return &model.GraphExcerpt{BlockID: node.BlockID, Text: node.name()}
	case node.text != "":
		return &model.GraphExcerpt{Text: node.name()}
	}
	return nil
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
// over the same ends (BacklinkCounts). Each predicate is bound through the gate (authorOnlyGate),
// so it reads `graph_nodes` only for an end that can be reserved for its author: the far end of
// an edge, whose kind is a column, is tested on its kind in SQL first, and an end whose kind is a
// literal of the build - a block of the node read, or the block holding an edge out of it - binds
// its kind's predicate only where the gate keeps it (gatedKind): a document's block does, and a
// block of an ask, a comment or a message, which the store reserves for nobody, binds none.
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
//
// An inbound read compares the far end with its id as a value Postgres reads once, before the
// read, `(select $1::text)`, and not with $1 itself, so the plan it makes for the id in hand is the
// plan it makes for any id. With $1, the custom plan of a node with a handful of edges into it is
// costed far below the generic plan, whose estimate of the edges into a node is the store's
// average (the 5,000 into one node of the read-cost corpus among them), while the custom plan of
// that 5,000-edge node costs far above it. Postgres keeps the generic plan or a custom plan for
// each execution by comparing the two costs over the executions each pooled connection has made of
// the statement, so which plan a read got depended on which nodes that connection happened to read
// first: a connection that had read the 5,000-edge node kept the generic plan, 1.5 ms; one that
// had read only small nodes planned the whole view again for every execution, 20 ms (on this pod,
// for a node with 7 edges into it). With the id read as a value, the custom plan is the generic
// plan, every connection keeps the generic plan from its sixth execution on, and that plan walks
// graph_tuples_as_of_in and the arms' own indexes keyed on the far end, as the custom plan did
// (check c-read-cost).
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
		// artifact by its uuid (edgeFrom).
		from := edgeFrom("g")
		source += ` and ` + gatedEnd(AnyKind, "g.from_kind", from, false, seen)
		own := source
		if itemBlocks {
			own += ` and not ` + repliesToABlockOf(repliesFrom(item), item, "g", blocks)
		}
		arms = `select g.kind, g.from_kind, ` + from + `, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = ` + kind + ` and g.to_id = (select ` + id + `::text) and ` + own
		switch {
		case document:
			arms += `
			union all
			` + keyedBlockArm("block", documentBlockIDs(id), source+` and `+gatedKind("block", "g.to_id", seen))
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
			` + keyedBlockArm(item.BlockKind(), itemBlockIDs(item, blocks), source+` and `+gatedKind(item.BlockKind(), "g.to_id", seen))
		}
	case query.Direction == "out" && document:
		// The document's own edges name it by its uuid, but the one the store states out of it
		// with no projection row behind it, which names it by its ref_key (edgeFrom), read through
		// that ref_key.
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id + ` and not ` + heldByABlock("g") +
			` and ` + gatedEnd(AnyKind, "g.to_kind", "g.to_id", true, seen) + `
			union all
			select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.kind = '` + graphstore.TupleChangedByRun + `'
			and g.from_id = (select a.ref_key from artifacts a where a.id = ` + blocks + `::uuid)` +
			` and ` + gatedEnd(AnyKind, "g.to_kind", "g.to_id", true, seen) + `
			union all
			select b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq, b.from_node
			from block_refs b where b.artifact_id = ` + blocks + `::uuid and ` + blockRefListed("b", "b.to_id") +
			` and ` + gatedKind("block", "b.from_node", seen) + ` and ` + gatedEnd(AnyKind, "b.to_kind", "b.to_id", true, seen)
	case query.Direction == "out" && itemBlocks:
		tables, _ := item.tables()
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id + ` and not ` + heldByABlock("g") +
			` and ` + gatedEnd(AnyKind, "g.to_kind", "g.to_id", true, seen) + `
			union all
			select r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq, r.from_node
			from ` + itemBlockLinks(item) + `
			where r.` + tables.item + ` = ` + blocks + `::uuid and ` + itemBlockCurrent +
			` and ` + gatedKind(item.BlockKind(), "r.from_node", seen) + ` and ` + gatedEnd(AnyKind, "r.to_kind", "r.to_id", true, seen)
	case query.Direction == "out":
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = ` + kind + ` and g.from_id = ` + id +
			` and ` + gatedEnd(AnyKind, "g.to_kind", "g.to_id", true, seen)
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
//
// It is the read of the node's own address, which answers two things no other read of the node
// does: a node whose own address answers its whole text (resolvedNode's wholeText: a run's words)
// carries it as its Text, from the same read and so behind the same predicate, and a run carries
// its frames (interactionFrames), read behind that predicate too. An edge at either shows only the
// head of that text (excerptOf), and an edge at a run no frame of it.
func Node(ctx context.Context, q Queryer, who model.Actor, kind, id string, byRefKey bool) (model.GraphNode, bool, error) {
	nodes, err := resolveNodes(ctx, q, who, byRefKey, []edgeRow{{otherKind: kind, otherID: id}})
	if err != nil {
		return model.GraphNode{}, false, err
	}
	node, found := nodes[[2]string{kind, id}]
	if node.wholeText {
		node.GraphNode.Text = node.text
	}
	if found && node.Kind == graphstore.KindInteraction {
		if node.Frames, err = interactionFrames(ctx, q, who, node.ID); err != nil {
			return model.GraphNode{}, false, err
		}
	}
	return node.GraphNode, found, nil
}

// Nodes resolves many nodes at once: each target resolved for who through its kind's loader, in
// one read per kind rather than one per node, and answered under the target it was asked by, as
// an edge at the node carries it - the whole text and the frames of a run are its own address's
// alone (Node). A target that names no node, and a node reserved for an author who is not who, is
// absent from the answer, exactly as Node answers it not found. The targets are addressed as the
// to side of an edge addresses them (TargetOf), so an artifact's id is its ref_key, as in
// TargetNames.
func Nodes(ctx context.Context, q Queryer, who model.Actor, targets []Target) (map[Target]model.GraphNode, error) {
	resolved, err := resolveTargets(ctx, q, who, targets)
	if err != nil {
		return nil, err
	}
	nodes := make(map[Target]model.GraphNode, len(resolved))
	for key, node := range resolved {
		nodes[Target{Kind: key[0], ID: key[1]}] = node.GraphNode
	}
	return nodes, nil
}

// ExcerptedNode is one node ExcerptedNodes resolves: the node, and the excerpt an edge at it
// shows (excerptOf), nil for a node that has no text.
type ExcerptedNode struct {
	Node    model.GraphNode
	Excerpt *model.GraphExcerpt
}

// ExcerptedNodes is Nodes with each node's excerpt beside it, cut as an edge at the node cuts it:
// a read that lists nodes rather than edges - the proposed nodes of an issue - answers each one
// with the words a reader recognises it by. What it answers and what it leaves out is Nodes'. The
// targets are addressed as the to side of an edge addresses them (TargetOf), so an artifact's id
// is its ref_key, as in TargetNames.
func ExcerptedNodes(ctx context.Context, q Queryer, who model.Actor, targets []Target) (map[Target]ExcerptedNode, error) {
	resolved, err := resolveTargets(ctx, q, who, targets)
	if err != nil {
		return nil, err
	}
	nodes := make(map[Target]ExcerptedNode, len(resolved))
	for key, node := range resolved {
		nodes[Target{Kind: key[0], ID: key[1]}] = ExcerptedNode{Node: node.GraphNode, Excerpt: excerptOf(node)}
	}
	return nodes, nil
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
// or a message's body, a component's title and a block's text, each cut to the bound an edge's
// excerpt is cut to (resolvedNode.name), which is what GET /api/v1/references already shows
// beside an edge at that node. The targets are addressed as the to side of an edge addresses
// them (TargetOf), so an artifact's id is its ref_key. A comment or a message, and a block of
// one, carries its post's marks beside its name (TargetName). A deleted post's node stands, with
// the empty body its delete left, so its name is "" beside its delete mark.
//
// One entry per target that resolves for who, as Nodes resolves it: a target with no entry is
// one nothing addresses now — its node was deleted or is gone, or its kind is one this build
// cannot address (resolveNodes) — or one who may not see, which each kind's loader leaves out as
// it leaves out a node that does not exist (visibility.go), a target reserved for an author who
// is not who among them, which answers as an address nobody holds does (Node). A caller draws
// either as a reference whose target is gone. A block of a deleted comment or message resolves to
// nothing too, since the delete indexes the empty body, which takes every block out of its post
// (api's planEditedItem, in clearDeletedPostBody), but it has an entry: Gone, with no name and
// its post's marks, the delete's and, where its author edited the post before deleting it, the
// last edit's time, as a block read of it is refused naming the deletion (api's
// itemBlockNotFound). That entry is read for who too, so a block of a post who may not see has
// none. Each kind is read in one statement whatever the number of targets, so a batch costs a
// statement per kind, not per target; a block of a comment or a message that resolves to
// nothing, whose post targets does not name too, costs one statement more per kind of post,
// which reads whether its post was deleted.
func TargetNames(ctx context.Context, q Queryer, who model.Actor, targets []Target) (map[Target]TargetName, error) {
	resolved, err := resolveTargets(ctx, q, who, targets)
	if err != nil {
		return nil, err
	}
	names := make(map[Target]TargetName, len(resolved))
	for key, node := range resolved {
		names[Target{Kind: key[0], ID: key[1]}] = TargetName{
			Name: node.name(), EditedAt: node.EditedAt, DeletedAt: node.DeletedAt, DeletedBy: node.DeletedBy,
		}
	}
	if err := deletedPostBlocks(ctx, q, who, targets, names); err != nil {
		return nil, err
	}
	return names, nil
}

// resolveTargets resolves each target for who through its kind's loader, as Node resolves one,
// addressed as the to side of an edge addresses it (TargetOf), so an artifact's id is its ref_key.
func resolveTargets(ctx context.Context, q Queryer, who model.Actor, targets []Target) (map[[2]string]resolvedNode, error) {
	rows := make([]edgeRow, len(targets))
	for index, target := range targets {
		rows[index] = edgeRow{otherKind: target.Kind, otherID: target.ID}
	}
	return resolveNodes(ctx, q, who, true, rows)
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
// excerpt, already cut. wholeText says a read of the node's own address answers that text whole
// as the node's Text (Node), which an interaction's loader says of a run's words
// (spec requirement r-interaction): an edge at the run cuts them at excerptRunes, and
// the run's own address answers every word of them.
type resolvedNode struct {
	model.GraphNode
	text      string
	refKey    string
	wholeText bool
}

// name is the words shown for the node in place of its text: its own text cut to excerptRunes. A
// block's text is its stored excerpt, already cut to BlockExcerptRunes.
func (n resolvedNode) name() string {
	if n.BlockID != "" {
		return n.text
	}
	return text.HeadRunes(n.text, excerptRunes)
}

// resolveNodes loads every distinct other end by kind, each loader answering only what who may
// see.
func resolveNodes(ctx context.Context, q Queryer, who model.Actor, byRefKey bool, rows []edgeRow) (map[[2]string]resolvedNode, error) {
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
		load := nodeRegistry[kind].load
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
// a role and a role's or a session's page (graphstore). Each is read from graph_nodes, and one
// the store has no row for, or holds gone, is absent from the answer, as every other loader leaves
// out a node that does not exist: a session Dispatch has recorded nothing of is no node of the
// graph, and the edges at it are omitted until a write records it. A node at any other state is
// present, and answers the state and the visibility its own row holds. None of the four carries a
// dispatch:// address or belongs to a node here: what a session is part of is a tuple, which the
// reads that show one follow for themselves.
func loadStoreNodes(ctx context.Context, q Queryer, who model.Actor, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select id, made_by, made_at, state, visibility from graph_nodes
		where kind = $1 and id = any($2) and state <> $3 and `+VisibleRow("graph_nodes", "$4"),
		kind, ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		node := model.GraphNode{Kind: kind}
		var madeBy []byte
		var madeAt time.Time
		if err := rows.Scan(&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility); err != nil {
			return fmt.Errorf("scan %s node: %w", kind, err)
		}
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			return err
		}
		nodes[[2]string{kind, node.ID}] = resolvedNode{GraphNode: node}
	}
	return rows.Err()
}

// loadIssueNodes resolves issue nodes. An issue is made by its creator; its last change is the
// latest event that wrote its own fields (issue.created, issue.updated, issue.closed: a claim, a
// deploy notice or a comment is no change of it), by that event's actor. It belongs to its parent
// issue, which has no versions.
func loadIssueNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, keys []string, nodes map[[2]string]resolvedNode) error {
	storedJoin, storedColumns := storedNode("issue", "i.key")
	rows, err := q.Query(ctx, `
		select i.key, i.project_key, i.title, i.created_by, i.created_at, i.parent_key, change.actor, change.created_at,
		       `+storedColumns+`
		from issues i
		left join lateral (
			select e.actor, e.created_at from events e
			where e.issue_key = i.key and e.type = any('{issue.created,issue.updated,issue.closed}')
			order by e.seq desc limit 1
		) change on true
		`+storedJoin+`
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
		var stored StoredNode
		if err := rows.Scan(&key, &project, &title, &createdBy, &createdAt, &parentKey, &changedBy, &changedAt, &stored.State, &stored.Visibility); err != nil {
			return fmt.Errorf("scan issue node: %w", err)
		}
		node := model.GraphNode{Kind: "issue", ID: key, IssueKey: new(key), Project: project, Ref: "dispatch://" + key, Parent: issueParent(parentKey)}
		if err := setMadeAndChanged(&node, createdBy, createdAt, changedBy, changedAt, storedActor); err != nil {
			return err
		}
		setStored(&node, stored)
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
//
// One artifact is bound as itself, as one block is (blockNodesAddressed): that is what every read
// of one node asks for - the document a block's address names, read before the block - and an
// equality on an index each of whose values is one row's (artifacts_ref_key_key for a ref_key,
// artifacts_id_text for a uuid) is a plan Postgres builds the same whether or not it knows the
// value, so it keeps one plan for the statement. Bound as an array of one, the plan made for the
// value in hand, which reads one element, was costed below the generic plan, which reads an array
// of unknown length as one of ten, so the read of one document was planned again on every
// execution: 0.5 to 1.0 ms of planning against 0.2 ms to run, over the read-cost corpus (check
// c-read-cost).
func loadArtifactNodes(ctx context.Context, q Queryer, who model.Actor, _ string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error {
	column := `a.id::text`
	if byRefKey {
		column = `a.ref_key`
	}
	var predicate string
	var addressed any
	if len(ids) == 1 {
		predicate, addressed = column+` = $1`, ids[0]
	} else {
		predicate, addressed = column+` = any($1)`, ids
	}
	storedJoin, storedColumns := storedNode("artifact", "a.ref_key")
	rows, err := q.Query(ctx, `
		select a.id::text, a.ref_key, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at, latest.authors, latest.created_at, `+storedColumns+`, a.retired_at is not null
		from artifacts a
		left join lateral (
			select v.authors, v.created_at from artifact_versions v
			where v.artifact_id = a.id order by v.number desc limit 1
		) latest on true
		`+storedJoin+`
		where `+predicate+` and `+VisibleKind("artifact", "a.ref_key", "$2"), addressed, VisibilityArg(who))
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
		var stored StoredNode
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &name, &kind, &primary, &createdBy, &createdAt, &authors, &versionAt, &stored.State, &stored.Visibility, &retired); err != nil {
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
		setStored(&node, stored)
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
// Its predicate and its stored state and visibility are read in one probe of its `graph_nodes`
// row rather than two (visibleStoredKind), since a read of a document's every block, or of the
// 5,000 blocks a list of links points from, answers thousands of them at once.
//
// Its joins to artifacts and artifact_versions are lateral probes by key (nodeLoader).
func loadBlockNodes(ctx context.Context, q Queryer, who model.Actor, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	held, args := blockNodesAddressed(ids, who)
	storedJoin, storedColumns, seen := visibleStoredKind("block", "d.node_id", "$2")
	rows, err := q.Query(ctx, `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at, changed.authors,
		       `+documentColumns("a")+`, `+blockFirstColumns("first")+`, `+storedColumns+`
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
		`+storedJoin+`
		where `+held+` and `+seen, args...)
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
		var stored StoredNode
		targets := append([]any{&nodeID, &blockID, &excerpt, &changedVersion, &changedAt, &changedBy}, document.targets()...)
		if err := rows.Scan(append(append(targets, first.targets()...), &stored.State, &stored.Visibility)...); err != nil {
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
		setStored(&node, stored)
		nodes[[2]string{"block", nodeID}] = resolvedNode{GraphNode: node, text: excerpt}
	}
	return rows.Err()
}

// blockNodesAddressed is how loadBlockNodes names the blocks it reads, and the arguments that
// go with it: the node ids bound as $1, the acting credential as $2, and for two or more ids the
// documents they name as $3.
//
// One block is bound as itself. That is what every read of one node asks for - the node a
// `dispatch://...#b-...` address names, and the far end of a chain step's one edge - and a
// single equality on the hash index over document_blocks.node_id is a plan the planner builds
// the same way whether or not it knows the value, so Postgres keeps one plan for the statement
// and the read stops paying to plan it again: measured on this pod over the read-cost corpus,
// 2.5 ms a call against 0.2 ms once the plan is the connection's.
//
// More than blockNodesKeptPlan arrive as an array, under QueryExecModeDescribeExec so that the
// statement is planned with its values. An array tells a generic plan nothing about its length,
// so that plan reads it as an unknown-length scalar-array operation rather than a constant it can
// hash: measured on this pod over a document of 8,001 blocks, 460 ms against 90 ms with the
// values in hand. They are also held to the documents they name (blockDocuments), which every
// one of them is a block of: IndexBlocks writes a row's node_id as the ref_key of the row's own
// document, '#' and its block id (blockNodeAddress), nothing rewrites it, and no ref_key changes
// once written. That narrows nothing, and it is there for the plan: it lets Postgres read a
// document's own rows by the primary key, which it does where the ids are thousands of blocks of
// a few documents, and keep the node_id index where they are few. Held to node_id alone, the
// 5,001 blocks of a list of links were read by scanning the whole table rather than its index,
// once the block predicate no longer sat on that scan to deter it (loadBlockNodes): 98 ms
// against 75 ms over the read-cost corpus (check c-read-cost). The documents are an array the
// read computes once (`artifact_id = any(array(...))`), and not a semi-join, whose selectivity the
// planner multiplies into the ids' own as if the two were unrelated: it then reckoned 36 of 640
// blocks of two documents, and scanned the whole table for them where the node_id index reads
// them. Measured on a copy of a dispatch:local database grown to the read-cost corpus (63,560
// blocks), 640 blocks of two documents read in 32 to 39 ms under the semi-join and 14 to 15 ms
// under the array, as the base build's read of node_id alone reads them (14 to 17 ms), and 5,000
// blocks of one document still read by the primary key under either.
//
// Two to blockNodesKeptPlan arrive as the same two arrays, each compared as a value Postgres reads
// once, before the read - `(select $1::text[])::text[]`, the cast making it the array itself
// rather than a subquery whose rows `any` would read - so the plan made for the ids in hand is the
// plan made for any of them, Postgres keeps it from the sixth execution on, and the read stops
// paying to plan it again. That plan reads the ids through the node_id index, as the plan made with
// the values does for so few: the far ends of the edges into one node, the 7 blocks citing a node
// of the read-cost corpus, are read in 0.4 ms against 2.3 ms planned each time.
func blockNodesAddressed(ids []string, who model.Actor) (string, []any) {
	switch {
	case len(ids) == 1:
		return `d.node_id = $1`, []any{ids[0], VisibilityArg(who)}
	case len(ids) <= blockNodesKeptPlan:
		return blockNodesKeptPlanPredicate, []any{ids, VisibilityArg(who), blockDocuments(ids)}
	}
	return blockNodesDescribedPredicate, []any{pgx.QueryExecModeDescribeExec, ids, VisibilityArg(who), blockDocuments(ids)}
}

// blockNodesByIDs is the predicate of a read of two or more blocks: the ids, $1, held to the
// documents they name, $3 (blockDocuments), as one array of their uuids the read computes once.
// blockNodesKeptPlanPredicate reads each array as a value Postgres reads once, before the read;
// blockNodesDescribedPredicate binds them as they are.
const blockNodesByIDs = `d.node_id = any(%[1]s) and d.artifact_id = any(array(select named.id from artifacts named where named.ref_key = any(%[2]s)))`

var (
	blockNodesKeptPlanPredicate  = fmt.Sprintf(blockNodesByIDs, `(select $1::text[])::text[]`, `(select $3::text[])::text[]`)
	blockNodesDescribedPredicate = fmt.Sprintf(blockNodesByIDs, `$1`, `$3`)
)

// blockNodesKeptPlan is the most blocks loadBlockNodes reads under one plan Postgres keeps rather
// than one it makes for the values in hand (blockNodesAddressed). Measured on this pod over the
// read-cost corpus, the kept plan read 2, 20 and 50 blocks in 0.3, 0.7 and 2.4 ms against 1.7, 4.6
// and 6.0 ms planned each time, and the two met at 100: 5.9 ms against 5.6.
const blockNodesKeptPlan = 100

// blockDocuments is the ref_key of the document each block node id of ids names, each once: the
// part of the id before its first '#', since a ref_key holds none.
func blockDocuments(ids []string) []string {
	held := make(map[string]struct{}, 2)
	documents := make([]string, 0, 2)
	for _, id := range ids {
		document, _, _ := strings.Cut(id, "#")
		if _, named := held[document]; named {
			continue
		}
		held[document] = struct{}{}
		documents = append(documents, document)
	}
	return documents
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
	storedJoin, storedColumns := storedNode("ask", "k.id::text")
	rows, err := q.Query(ctx, `
		select k.id::text, k.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), k.question,
		       k.author, k.created_at, k.edited_at, edit.edited_by, `+documentColumns("doc")+`,
		       case
		         when `+anchorArtifact+` is not null then `+anchorVersion("k")+`
		         when k.block_artifact_id is not null then case when first.known then first.version end
		         else k.written_version
		       end, `+storedColumns+`
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
		`+storedJoin+`
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
		var stored StoredNode
		targets := append([]any{&id, &issueKey, &project, &slug, &question, &author, &createdAt, &editedAt, &editedBy}, document.targets()...)
		if err := rows.Scan(append(targets, &version, &stored.State, &stored.Visibility)...); err != nil {
			return fmt.Errorf("scan ask node: %w", err)
		}
		node := model.GraphNode{Kind: "ask", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("ask", issueKey, project, slug, id)}
		if err := setMadeAndChanged(&node, author, createdAt, editedBy, editedAt, storedActor); err != nil {
			return err
		}
		setStored(&node, stored)
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
	storedJoin, storedColumns := storedNode("comment", "c.id::text")
	rows, err := q.Query(ctx, `
		select c.id::text, c.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), c.body,
		       c.author, c.created_at, c.edited_at, c.deleted_at, c.deleted_by, c.reply_to::text, c.replies_to_block, c.ask_id::text, `+documentColumns("doc")+`,
		       case when c.anchor->>'artifact_id' is not null then `+anchorVersion("c")+` else c.written_version end,
		       `+storedColumns+`
		from comments c
		left join lateral (select i.project_key from issues i where i.key = c.issue_key limit 1) i on true
		left join lateral (select a.project_key, a.slug from artifacts a where a.id = c.artifact_id limit 1) a on true
		left join lateral (
			select doc.id, doc.issue_key, doc.project_key, doc.session_id, doc.slug, doc.kind, doc.is_primary
			from artifacts doc where doc.id = coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id) limit 1
		) doc on true
		`+storedJoin+`
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
		var stored StoredNode
		targets := append([]any{&id, &issueKey, &project, &slug, &body, &author, &createdAt, &editedAt, &deletedAt, &deletedBy, &replyTo, &replyBlock, &askID}, document.targets()...)
		if err := rows.Scan(append(targets, &version, &stored.State, &stored.Visibility)...); err != nil {
			return fmt.Errorf("scan comment node: %w", err)
		}
		node := model.GraphNode{Kind: "comment", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("comment", issueKey, project, slug, id), EditedAt: editedAt}
		if err := setMadeAndChanged(&node, author, createdAt, author, editedAt, storedActor); err != nil {
			return err
		}
		setStored(&node, stored)
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
	storedJoin, storedColumns := storedNode("message", "m.id::text")
	rows, err := q.Query(ctx, `
		select m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body, m.author, m.created_at, m.edited_at,
		       m.deleted_at, m.deleted_by, m.in_reply_to::text, m.replies_to_block, `+storedColumns+`
		from messages m
		left join lateral (select i.project_key from issues i where i.key = m.issue_key limit 1) i on true
		`+storedJoin+`
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
		var stored StoredNode
		if err := rows.Scan(&id, &issueKey, &project, &body, &author, &createdAt, &editedAt, &deletedAt, &deletedBy, &inReplyTo, &replyBlock,
			&stored.State, &stored.Visibility); err != nil {
			return fmt.Errorf("scan message node: %w", err)
		}
		node := model.GraphNode{Kind: "message", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("message", issueKey, project, "", id), EditedAt: editedAt}
		if err := setMadeAndChanged(&node, author, createdAt, author, editedAt, storedActor); err != nil {
			return err
		}
		setStored(&node, stored)
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
	storedJoin, storedColumns := storedNode("component", "c.project_key || '/' || c.id")
	rows, err := q.Query(ctx, `
		select c.project_key, c.id, c.title, f.made_at, f.changed_at, p.id, `+storedColumns+`
		from components c
		left join component_facts f on f.project_key = c.project_key and f.id = c.id
		left join components p on p.project_key = c.project_key and p.id = c.parent
		`+storedJoin+`
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
		var stored StoredNode
		if err := rows.Scan(&project, &id, &title, &madeAt, &changedAt, &parent, &stored.State, &stored.Visibility); err != nil {
			return fmt.Errorf("scan component node: %w", err)
		}
		address := project + "/" + id
		node := model.GraphNode{
			Kind: "component", ID: address, Project: project, Ref: componentRef(project, id),
			MadeBy: []model.Actor{}, MadeAt: madeAt, ChangedBy: []model.Actor{}, ChangedAt: changedAt,
		}
		setStored(&node, stored)
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
