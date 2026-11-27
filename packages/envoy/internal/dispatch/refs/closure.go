package refs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Member is a document of an issue's Links, one its own content links to, with that row's via,
// or the lens of Links a linked document's address moved to. The read reads one hop, so Depth is
// always 1; the answer's members field keeps it for the clients that print it.
type Member struct {
	// Kind is the member's kind: graphstore.KindArtifact for a document, or graphstore.KindLens for
	// the lens a link to a document reaches where the document's address moved to it, as the
	// pre-mortem conversion moves the address of the document it retires (api.ConvertPremortem).
	Kind string
	// Address is the member's dispatch:// address: the document's (ArtifactRef) or the lens's.
	Address string
	// RefKey is the ref_key of the document the link reaches: the member itself, or, for a lens,
	// the retired document whose address moved to it.
	RefKey string
	Depth  int
	Via    model.ReferenceVia
}

// References is an issue's references read: Links, every node the issue's own content links to;
// LinkedFrom, every node that links to the issue or to its own content; and Members, the
// documents of Links. When Closure answers no error, Links and LinkedFrom are never nil, so the
// answer writes [] for a list with no row.
type References struct {
	Members           []Member
	Links, LinkedFrom []model.ReferenceLink
}

// documentTarget is true of a link row r whose target is a document, or a block address that
// resolves to a block a current version of its document holds (ResolvedBlockSQL: its moved_to
// tuples first, then its document's room, then the address itself), or a block address whose
// moved_to tuples reach an ask whose row stands instead; documentOfTarget is that document's
// ref_key, which never holds a '#', and r's own target for any other kind: for a block target, the
// document holding the block the target resolved to, so a citation of a block that moved reaches
// the document it moved into (R8), and a citation of a block of a document converted into its
// issue's room, at the document's own address, reaches that document, whose fragment of the room
// holds the block, never the room's tree, whose ref_key the node's own address carries; and the
// document the row of an ask a move ends at names, so a citation of the heading of a section that
// cites an answered ask a person removed reaches the document that ask was decided in.
// documentBlockHeld is the join that answers what a block target resolves to (movedTarget),
// block_target, and documentTarget reads its answer rather than an `exists` probe of its own:
// under an OR the probe is no semijoin, so the planner may plan it as a hashed subplan and read
// all of document_blocks to build the hash. A lateral whose probe stops at its first row is a
// nested loop keyed through document_blocks_node_id, the hash index on exactly node_id, and it
// runs at all only for a block target, because its own to_kind test is a one-time filter the
// planner re-reads on each loop. Only a target that probe does not answer is resolved through
// block_moved_to, and only one that resolves to no block through moved_to_reach, so a block that
// stands costs the probe alone.
var (
	documentBlockHeld = `left join lateral (` + movedTarget(`r.to_id`) + `
				where r.to_kind = 'block'
			) block_target on true`
	documentTarget   = `(r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.node_id is not null or block_target.ask_document is not null))`
	documentOfTarget = `case when r.to_kind = 'block' then coalesce(block_target.ref_key, block_target.ask_document) else r.to_id end`
)

// documentMovedTo is the join that answers, for a link row r whose target is a document, the lens
// the document's address moved to (block_moved_to, which reads from a document's address the
// moved_to the pre-mortem conversion appends to the lens it retires the document into), where
// the acting person, bound at param, may see that lens: moved_document.lens_id, null for every
// other row. A document whose lens the reader may not see stays the document, as it reads to a
// reader who sees no move, so the read says nothing of that lens. Like documentBlockHeld's, its
// to_kind test is a one-time filter, so the function runs for a document target alone.
func documentMovedTo(param string) string {
	return `left join lateral (
				select moved.lens_id from block_moved_to(r.to_id) as moved (lens_id)
				where r.to_kind = 'artifact' and moved.lens_id is not null
				  and ` + VisibleKind(graphstore.KindLens, `moved.lens_id`, param) + `
			) moved_document on true`
}

// heldBlockDocument is the ref_key of the document holding the current block whose node id the
// SQL expression node names: the document whose fragment of an issue room holds it
// (issue_room_blocks), for a block of an issue room, whose node's address carries the room's tree,
// else the document its node's address names before its '#'.
func heldBlockDocument(node string) string {
	return `coalesce(
					(select owner.ref_key from document_blocks held
					 join issue_room_blocks room_block on room_block.tree_artifact_id = held.artifact_id
					   and block_id_key(room_block.block_id) = block_id_key(held.block_id) and room_block.block_id = held.block_id
					 join artifacts owner on owner.id = room_block.document_id
					 where held.node_id = ` + node + ` limit 1),
					split_part(` + node + `, '#', 1))`
}

// movedTarget is the select list and FROM of what the block address toID resolves to, as
// (node_id, ref_key, ask_id, ask_document): node_id is the block the address resolves to
// (ResolvedBlockSQL) and ref_key the document holding it (heldBlockDocument); where the address
// resolves to no block and its moved_to tuples reach an ask whose row stands instead
// (moved_to_reach), ask_id is that ask's id and ask_document the ref_key of the document holding
// the ask's block (AskBlockDocumentSQL). Each is null where it resolves to none. moved_to_reach
// runs only where the resolution found no block: its subquery's own test of that is a one-time
// filter, kept one by `offset 0`, which stops the planner pulling the subquery up into the join.
func movedTarget(toID string) string {
	return `
				select resolved.node_id, ` + heldBlockDocument(`resolved.node_id`) + ` as ref_key,
				       case when moved.kind = 'ask' then moved.id end as ask_id,
				       case when moved.kind = 'ask' then (
				           select asked_in.ref_key from asks moved_ask
				           join artifacts asked_in on asked_in.id = ` + AskBlockDocumentSQL("moved_ask") + `
				           where moved_ask.id = moved.id::uuid) end as ask_document
				from (select ` + ResolvedBlockSQL(toID) + ` as node_id) resolved
				left join lateral (
					select reach.kind, reach.id from moved_to_reach(` + toID + `) reach where resolved.node_id is null offset 0
				) moved on true`
}

// itemBlockHeld is the join that reduces a link row r whose target is a block of an ask, a
// comment or a message to that item, while the block is one of the item's current ones
// (itemBlockCurrent): item_block is the item's kind and uuid, null for any other target and for
// a block its item has removed. Each arm reads its blocks table through its hash index on
// node_id, and only for its own block kind, whose to_kind test is a one-time filter.
var itemBlockHeld = func() string {
	arms := make([]string, len(itemKinds))
	for index, item := range itemKinds {
		tables, _ := item.tables()
		arms[index] = `select '` + string(item) + `', b.` + tables.item + `::text from ` + tables.blocks + ` b
				where r.to_kind = '` + item.BlockKind() + `' and b.node_id = r.to_id and ` + itemBlockCurrent
	}
	return `left join lateral (
				` + strings.Join(arms, `
				union all
				`) + `
				limit 1
			) item_block (kind, id) on true`
}()

// itemBlockEnds is every kind of node a block of an item is: the kinds the target of a link row
// that itemBlockHeld reduces to its item can be, none of them a standing kind.
var itemBlockEnds = func() NodeKinds {
	kinds := make([]string, len(itemKinds))
	for index, item := range itemKinds {
		kinds[index] = item.BlockKind()
	}
	return KindsOf(kinds...)
}()

// targetNode is the kind and the id of the node the target of a link row r counts as, beside
// itemBlockHeld: a document whose address moved to a lens, the lens whose id the SQL expression
// lens names (null where none: documentMovedTo), is that lens; a document's block is its
// document, by the ref_key document names (documentOfTarget beside documentBlockHeld, or the
// in statement's heldBlockDocument), and a block of an item its item; any other target is itself.
func targetNode(document, lens string) string {
	return `case when ` + lens + ` is not null then '` + graphstore.KindLens + `'
				         else coalesce(item_block.kind, case when r.to_kind = 'block' then 'artifact' else r.to_kind end) end,
				       coalesce(` + lens + `, item_block.id, ` + document + `)`
}

// oneRowPerNode ends both statements of the read. far holds a row for each link: the node at its
// far end (node_kind, node_id), its kind, its via at the near end, and moved_from, the ref_key of
// the document whose address moved to a lens at the far end, null for every other row. It leaves
// out the issue and its own content, the rows of own, the relation that holds them as (kind, id),
// and keeps one row for each other node: the distinct kinds of its links in order, its lowest via,
// the least pair of via kind and via id as arrays compare, element by element in the database's
// order, and the least of its moved_from.
//
// It leaves them out by an anti-join, `not exists` over own, and not, as the read did before, by a
// test of each row against an array of own's rows,
// `not (row(f.node_kind, f.node_id) = any(array(...)))` over `select row(o.kind, o.id) from own o`
// in the out statement and over `select row(t.kind, t.id) from targets t` in the in statement.
// Postgres pushed the array's test into each arm of the links, and it does not hash an array an
// InitPlan reads, so it compared each link with every row of own in turn. On a copy of a
// dispatch:local database seeded at production's size (block_refs 876,018 rows), for an issue
// with 2,041 own targets, the in statement read 5,000 links in 3.44-3.48 s, past the route's 2 s
// budget, and its plan spent 2.58 s of 2.67 s in that test; with an anti-join, hashed or merged,
// it read them in 0.27-0.36 s. On the large issue of api's
// TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks, 2,042 own targets and 10,000
// links from 500 documents, the read with the array answered 503 at 2.01 s in a shared pod and 200
// in 931 ms on a 10-CPU build machine, so the time of one read is no proof: api's
// TestTheReferencesInStatementLeavesTheIssuesOwnNodesOutByAnAntiJoin holds the plan, and that
// test holds how the time grows with the links (referencesGrowthBound).
func oneRowPerNode(own string) string {
	return `
		select f.node_kind, f.node_id, array_agg(distinct f.kind order by f.kind),
		       (min(array[f.via_kind, f.via_id]))[1], (min(array[f.via_kind, f.via_id]))[2], min(f.moved_from)
		from far f
		where not exists (select 1 from ` + own + ` x where x.kind = f.node_kind and x.id = f.node_id)
		group by f.node_kind, f.node_id`
}

// closureSeen is the parameter both statements of the read bind the acting credential at
// (VisibilityArg), which every predicate they render names; $1 is the issue's key.
const closureSeen = "$2"

// closureLinkKinds is the parameter both statements of the read bind the pointer kinds of the
// installed block schema at (pmdoc.LinkKinds), the kinds blockRefLinked admits beside a mention.
const closureLinkKinds = "$3"

// blockRefLinked is whether the block_refs row alias is a link of the references read: a
// mention, or a typed block's `block[]` pointer of a kind the installed block schema declares
// (closureLinkKinds) while the graph read lists it (blockRefListed), whether or not the row names
// its node. block_refs also holds a decision's ties that are neither: its decided_by, the
// `actor` attribute the server writes, to the person or the session who decided, and its
// from_ask, an `ask` attribute, to the ask it settles. block_ref_listed holds of both, since
// neither attribute declares targets, and graph_edges and the graph read list both (the census
// reads graph_edges' decided_by), so blockRefListed stays the view's rule and this read adds the
// test of the kind: the person who decided is no reference of the issue. The kinds are bound at
// each read and not spelled into the statement, which is rendered once, because the installed
// schema is the store's and is installed again whenever the store's changes
// (schemanodes.Installer).
func blockRefLinked(alias string) string {
	return `(` + alias + `.kind = 'mentions' or ` + alias + `.kind = any(` + closureLinkKinds + `::text[])) and ` +
		blockRefListed(alias, alias+`.to_id`)
}

// linksOutQuery is the out statement of the references read of the issue whose key is $1: every
// node the issue's own content links to, read from the links of that content by their source.
// The issue's own content is its documents and its items, those of each item kind (itemKinds:
// its comments, its messages and its asks), each read by the issue's key. Its links are each
// one's own refs rows, read by refs' primary key (from_kind, from_id); every link of its
// documents' blocks, mentions and declared pointers (blockRefLinked), read from block_refs by
// its primary key's document; and the links of the current blocks of each item whose own refs
// rows do not hold every mention its blocks hold (ItemKind.refsHoldBlockLinks), an ask's parts,
// read from the kind's block mentions table by its primary key's item (itemBlockLinks). A
// comment's or a message's own refs rows hold every mention its blocks hold, so their block links
// need no read of their own. A source whose block links are read so, a document or such an item,
// counts its own mention of a target a block of it holds as that block's link and not again
// (heldByABlock), so a pointer's target takes the pointer's kind alone. Each read is a lateral
// for one source, which `offset 0` keeps a nested loop keyed by that source: pulled up into a
// join, it could be planned as a hash or a merge join over a whole read of its table. Each item
// kind's items are read once by the issue's key (own_<its items table>), for its sources and for
// the read of its block links alike. A row of block_refs is listed as blockRefLinked lists it,
// whether or not it names its node. The blocks of an issue room are keyed by its tree artifact, a
// document of the issue, so its block links are read under the tree, and none under a document
// converted into the room, whose own rows are none of its blocks (documentBlockIDs).
//
// heldByABlock probes block_refs by the mention's target and its document's id as text
// (`artifact_id::text = from_id`), which block_refs_to_source keys by its three columns, to_kind,
// to_id and artifact_id::text: an Index Cond on all three, which reads the one row it looks for.
// A probe by the document's uuid (`artifact_id = <uuid>`) was planned through the same index's
// first two columns with artifact_id left as a Filter, at a cost of 8.43 against 21.20 for
// block_refs' primary key, which needs a skip scan over block_id. On the fixture of
// TestTheReferencesReadReadsOnlyTheIssuesOwnLinks, where other documents' blocks link the
// targets the issue's documents link, it removed 342 rows a probe, and the read's statements
// read 6,338.98 rows of the link tables against the 1,730 the test allows.
//
// The far end of a link is reduced to its node (targetNode): a document's block to the document
// holding the block a current version holds that the target resolves to, or to the document of
// the block or the ask its moved_to tuples reach (documentTarget), a block of an ask, a comment or
// a message to its item while the block is current (itemBlockHeld), and a document whose address
// moved to a lens the reader may see to that lens (documentMovedTo), whose row names the document
// as moved_from; a link to a block neither holds names nothing. A document is named by its
// ref_key, which a link stores. The issue and its own content are left out (own): the issue by
// its key, a document by its ref_key and every other source as sources names it. Its rows are one
// for each node, with the kinds of its links and its lowest via: a document of the issue by its
// uuid, the id its links are written under, an item by its uuid (oneRowPerNode).
//
// A link is read only where the acting person, whose credential is bound at $2 (closureSeen), may
// see its source and the block it is read at, as refs.Edges' out read binds them: a source is a
// document or an item of the issue they may see, each one's predicate read once, as a column of
// its own_ relation (visible), and a link a block holds is read only from a block they may see.
// Its target is held to the block the row's target resolves to and the ask a move of that block
// ends at, and to the block of an item it names, where it names one; the node at its far end, the
// document such a block counts as among them, is answered only where they may see it by
// resolveNodes, which binds that node kind's predicate. So a node reserved for another author is
// neither listed nor named as the via of a node that is. The statement is rendered at its first
// use and not as the variable is initialized, because the predicates it binds read the registry,
// which init fills after every variable of the package (nodeRegistry): rendered before that,
// every predicate of a kind would be `false` and the read empty.
var linksOutQuery = sync.OnceValue(func() string {
	ownItems := make([]string, 0, len(itemKinds))
	sources := []string{`select 'artifact', o.id::text from own_documents o where o.visible`}
	// blockLinksRead is every kind of source whose block links an arm of links reads: a
	// document's, and an item's whose own refs rows do not hold them.
	blockLinksRead := []string{`'artifact'`}
	blockLinks := []string{`select r.kind, r.to_kind, r.to_id, 'artifact', o.id::text
			from own_documents o
			cross join lateral (
				select r.kind, r.to_kind, r.to_id from block_refs r
				where r.artifact_id = o.id and ` + blockRefLinked("r") + `
				  and ` + VisibleKind(BlockNodeKind, `r.from_node`, closureSeen) + `
				offset 0
			) r
			where o.visible and not exists (select 1 from document_rooms moved where moved.artifact_id = o.id)`}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		own := `own_` + tables.items
		ownItems = append(ownItems, own+` as (
			select x.id, `+VisibleKind(string(item), `x.id::text`, closureSeen)+` as visible from `+tables.items+` x where x.issue_key = $1
		)`)
		sources = append(sources, `select '`+string(item)+`', o.id::text from `+own+` o where o.visible`)
		if item.refsHoldBlockLinks() {
			continue
		}
		blockLinksRead = append(blockLinksRead, `'`+string(item)+`'`)
		blockLinks = append(blockLinks, `select r.kind, r.to_kind, r.to_id, '`+string(item)+`', o.id::text
			from `+own+` o
			cross join lateral (
				select r.kind, r.to_kind, r.to_id from `+itemBlockLinks(item)+`
				where r.`+tables.item+` = o.id and `+itemBlockCurrent+`
				  and `+VisibleKind(item.BlockKind(), `r.from_node`, closureSeen)+`
				offset 0
			) r
			where o.visible`)
	}
	return `
		with own_documents as (
			select a.id, a.ref_key, ` + VisibleKind("artifact", `a.ref_key`, closureSeen) + ` as visible
			from artifacts a where a.issue_key = $1
		),
		` + strings.Join(ownItems, `,
		`) + `,
		sources (kind, id) as (
			` + strings.Join(sources, `
			union all `) + `
		),
		own (kind, id) as (
			select 'issue', $1::text
			union all select 'artifact', o.ref_key from own_documents o
			union all select s.kind, s.id from sources s where s.kind <> 'artifact'
		),
		links (kind, to_kind, to_id, via_kind, via_id) as (
			select r.kind, r.to_kind, r.to_id, s.kind, s.id
			from sources s
			cross join lateral (
				select r.kind, r.to_kind, r.to_id from refs r
				where r.from_kind = s.kind and r.from_id = s.id
				  and (s.kind not in (` + strings.Join(blockLinksRead, ", ") + `) or not ` + heldByABlock("r") + `)
				offset 0
			) r
			union all
			` + strings.Join(blockLinks, `
			union all
			`) + `
		),
		far (node_kind, node_id, kind, via_kind, via_id, moved_from) as (
			select ` + targetNode(documentOfTarget, `moved_document.lens_id`) + `, r.kind, r.via_kind, r.via_id,
			       case when moved_document.lens_id is not null then r.to_id end
			from links r
			` + documentBlockHeld + `
			` + documentMovedTo(closureSeen) + `
			` + itemBlockHeld + `
			where (` + documentTarget + ` or r.to_kind not in ('artifact', 'block'))
			  and (r.to_kind not in ` + itemBlockKinds + ` or item_block.id is not null)
			  and (r.to_kind <> 'block' or ` + VisibleKind(BlockNodeKind, `block_target.node_id`, closureSeen) +
		` and ` + VisibleKind(string(ItemAsk), `block_target.ask_id`, closureSeen) + `)
			  and (r.to_kind not in ` + itemBlockKinds + ` or ` + VisibleEnd(itemBlockEnds, `r.to_kind`, `r.to_id`, true, closureSeen) + `)
		)` + oneRowPerNode(`own`)
})

// linksInQuery is the in statement of the references read of the issue whose key is $1: every
// node that links to the issue or to its own content, read from the links into them by their
// target. It reads the issue's own targets once (targets): the issue node by its key, its
// documents by ref_key and their current blocks (own_blocks: document_blocks, by its primary
// key's document), and its asks, comments and messages by the issue's key and their current
// blocks (itemBlockCurrent). Then it reads each link table once for each kind of target, `to_kind
// = <kind> and to_id = any(<the ids of that kind>)` (keys), through the table's index of targets
// (refs_to, block_refs_to_source, ask_block_refs_to, comment_block_refs_to,
// message_block_refs_to), in a lateral that `offset 0` keeps a nested loop keyed by the kind, as
// the out statement keeps its reads: a source block of an item while it is current
// (itemBlockLinks, itemBlockCurrent), a block's mention and its declared pointer while
// block_ref_listed lists it (blockRefLinked), and an item's own mention of a target
// that a block of that item holds as that block's link alone (heldByABlock).
//
// The blocks of an issue room are keyed by its tree artifact, a document of the issue, so they
// are read under the tree, and none under a document converted into the room, whose own rows are
// none of its blocks (documentBlockIDs). A reference to a block of a converted document is
// written at that document's own address (`<ref_key>#<block id>`), which is no node id but
// resolves to the room's node (ResolvedBlockSQL), so every such address of a block of the room,
// at each document edited in it, is a target too (room_addresses, as documentRoomBlockAddresses
// spells one document's), and a link to one counts as a link to the block it resolves to, while
// that block is one of own_blocks (addressed).
//
// Each link's source is reduced to its node: a document's block to its document, the one holding
// it for a block of an issue room (BlockDocumentSQL), and a block of an item to the item. A source
// document, or a source block's document, is named by its ref_key, read from artifacts by its id
// in a lateral that stops at its first row, as the out statement names a document, so one naming
// pass serves both. Each link's target is reduced to the node it counts as (targetNode), its via:
// the id a link to that node stores, the issue's key, a document's ref_key, the document holding a
// block (heldBlockDocument), or an item's uuid. The issue and its own content are left out
// (targets). Its rows are one for each node, with the kinds of its links and its lowest via
// (oneRowPerNode).
//
// A target is read only where the acting person, whose credential is bound at $2 (closureSeen),
// may see it and the node it counts as: the issue itself where they may see it, a document or an
// item of the issue they may see, and a block of one they may see that they may see too, as
// refs.Edges' in read holds the node read and the blocks it reads at. A link a block holds is read
// only from a block they may see; the node at its near end, the source, is answered only where
// they may see it by resolveNodes, which binds that node kind's predicate. The statement is
// rendered at its first use, as linksOutQuery is.
var linksInQuery = sync.OnceValue(func() string {
	visibleDocument := VisibleKind("artifact", `o.ref_key`, closureSeen)
	targets := []string{
		`select 'issue', $1::text where ` + VisibleKind("issue", `$1::text`, closureSeen),
		`select 'artifact', o.ref_key from own_documents o where ` + visibleDocument,
		`select 'block', b.node_id from own_blocks b`,
		`select 'block', a.address from room_addresses a`,
	}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		visibleItem := VisibleKind(string(item), `x.id::text`, closureSeen)
		targets = append(targets,
			`select '`+string(item)+`', x.id::text from `+tables.items+` x where x.issue_key = $1 and `+visibleItem,
			`select '`+item.BlockKind()+`', b.node_id
			from `+tables.items+` x join `+tables.blocks+` b on b.`+tables.item+` = x.id
			where x.issue_key = $1 and `+itemBlockCurrent+` and `+visibleItem+`
			  and `+VisibleKind(item.BlockKind(), `b.node_id`, closureSeen))
	}
	// byTarget reads the links of from, its link table aliased r, that end at a target of each
	// kind of keys and meet condition, as rows of their source node (columns), their kind and
	// their target.
	byTarget := func(columns, from, condition string) string {
		return `select r.node_kind, r.node_id, r.kind, r.to_kind, r.to_id
			from keys k
			cross join lateral (
				select ` + columns + `
				from ` + from + `
				where r.to_kind = k.kind and r.to_id = any(k.ids) and ` + condition + `
				offset 0
			) r (node_kind, node_id, kind, to_kind, to_id)`
	}
	links := []string{
		byTarget(`r.from_kind, case when r.from_kind = 'artifact' then src.ref_key else r.from_id end, r.kind, r.to_kind, r.to_id`,
			`refs r
				left join lateral (
					select a.ref_key from artifacts a where r.from_kind = 'artifact' and a.id::text = r.from_id limit 1
				) src on true`,
			`not `+heldByABlock("r")),
		byTarget(`'artifact', src.ref_key, r.kind, r.to_kind, r.to_id`,
			`block_refs r
				cross join lateral (select a.ref_key from artifacts a where a.id = `+BlockDocumentSQL("r")+` limit 1) src`,
			blockRefLinked("r")+` and `+VisibleKind(BlockNodeKind, `r.from_node`, closureSeen)),
	}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		links = append(links, byTarget(`'`+string(item)+`', r.`+tables.item+`::text, r.kind, r.to_kind, r.to_id`,
			itemBlockLinks(item), itemBlockCurrent+` and `+VisibleKind(item.BlockKind(), `r.from_node`, closureSeen)))
	}
	return `
		with own_documents as (
			select a.id, a.ref_key from artifacts a where a.issue_key = $1
		),
		own_blocks (node_id) as (
			select d.node_id from own_documents o join document_blocks d on d.artifact_id = o.id
			where ` + visibleDocument + ` and ` + VisibleKind(BlockNodeKind, `d.node_id`, closureSeen) + `
			  and not exists (select 1 from document_rooms moved where moved.artifact_id = o.id)
		),
		room_addresses (address) as (
			select joined_document.ref_key || '#' || room_block.block_id
			from own_documents o
			join issue_room_blocks room_block on room_block.tree_artifact_id = o.id
			join document_rooms joined on joined.room = o.id
			join artifacts joined_document on joined_document.id = joined.artifact_id
		),
		targets (kind, id) as (
			` + strings.Join(targets, `
			union all
			`) + `
		),
		keys (kind, ids) as (
			select t.kind, array_agg(t.id) from targets t group by t.kind
		),
		links (node_kind, node_id, kind, to_kind, to_id) as (
			` + strings.Join(links, `
			union all
			`) + `
		),
		far (node_kind, node_id, kind, via_kind, via_id, moved_from) as (
			select r.node_kind, r.node_id, r.kind,
			       ` + targetNode(`case when r.to_kind = 'block' then `+heldBlockDocument(`coalesce(addressed.node_id, r.to_id)`)+` else r.to_id end`, `null::text`) + `,
			       null::text
			from links r
			cross join (select array(select b.node_id from own_blocks b) as ids) own_block
			left join lateral (
				select ` + ResolvedBlockSQL(`r.to_id`) + ` as node_id
				where r.to_kind = 'block' and not (r.to_id = any(own_block.ids))
				offset 0
			) addressed on true
			` + itemBlockHeld + `
			where r.node_id is not null
			  and (r.to_kind <> 'block' or coalesce(addressed.node_id, r.to_id) = any(own_block.ids))
		)` + oneRowPerNode(`targets`)
})

// ClosureTag starts every statement Closure sends, so that the query text pg_stat_activity shows
// for any statement of the read, the loaders' included, names it. A caller that sends statements
// of its own inside the read, as the route's budget sends its statement_timeout before each one
// (api's setStatementBudget), starts them with it too.
const ClosureTag = "/* refs.Closure */\n"

// taggedQueryer passes every statement on to the Queryer it wraps with ClosureTag before it.
type taggedQueryer struct{ Queryer }

func (q taggedQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.Queryer.Query(ctx, ClosureTag+sql, args...)
}

func (q taggedQueryer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.Queryer.QueryRow(ctx, ClosureTag+sql, args...)
}

// linkRow is one row of the out or the in statement: the node at the far end, as resolveNodes
// addresses it with byRefKey (a document by its ref_key), the kinds of its links, its via, and,
// for a lens a linked document's address moved to, that document's ref_key (movedFrom).
type linkRow struct {
	kind, id  string
	kinds     []string
	via       model.ReferenceVia
	movedFrom *string
}

// Closure reads the references of the issue whose key is issueKey, one hop in each direction:
// Links, every node the issue's own content links to (linksOutQuery), and LinkedFrom, every node
// that links to the issue or to its own content (linksInQuery). It sends the out statement, then
// the in statement, then names the nodes of both through resolveNodes, the resolver the graph
// read names its nodes with, at most one statement for each kind of node. A row whose node
// resolveNodes does not name, a node gone since or of a kind this build does not know, is left
// out, as Edges leaves such an edge out. Each list is in the order of the node's kind, then its
// id. Every statement it sends, through q, starts with ClosureTag. A link is a mention, or a typed
// block's pointer of a kind the installed schema declares while the graph read lists it
// (blockRefLinked): a decision's decided_by and from_ask, which block_refs holds beside them, are
// neither and are no link. serverURL is the deployment whose dashboard URLs are references, which
// a node's name is cut around (resolvedNode.name).
//
// Members are the documents of Links, each at depth 1 with its row's via, by ref_key, then by
// kind: a link to a block reaches the document holding the block while a current version of the
// document holds that block, and a link to a block that moved reaches the document of the block
// its moved_to tuples reach (graphstore.MovedTo), or, where a move ends at an ask, the document
// the ask's row names, the document it was decided in. A link to a document whose own address
// moved to a lens - the pre-mortem conversion retires a document into one and moves its address
// there (api.ConvertPremortem) - reaches that lens, which Links lists, and the member is of kind
// lens at the lens's address, with the retired document's ref_key. A link to a block of that
// document is no link to its address, and still reaches the document itself, so one document can
// stand for two members, the lens and itself. A retired document whose address moved to no lens
// the reader may see names its issue (loadArtifactNodes), which each list holds in its place, once
// however many of its rows name it (related), so it is no member.
//
// It answers for who: a link whose source, or the block it is read at, who may not see is no
// link of the read, and a node who may not see is neither listed nor named as a via, so a node
// reserved for another author never shows in either list or among the members (VisibleKind,
// VisibleEnd, resolveNodes).
func Closure(ctx context.Context, q Queryer, who model.Actor, issueKey, serverURL string) (References, error) {
	tagged := taggedQueryer{q}
	args := closureArgs(issueKey, who)
	out, err := readLinkRows(ctx, tagged, linksOutQuery(), args)
	if err != nil {
		return References{}, fmt.Errorf("read the links out of %s's own content: %w", issueKey, err)
	}
	in, err := readLinkRows(ctx, tagged, linksInQuery(), args)
	if err != nil {
		return References{}, fmt.Errorf("read the links into %s and its own content: %w", issueKey, err)
	}
	lookups := make([]edgeRow, 0, len(out)+len(in))
	for _, rows := range [...][]linkRow{out, in} {
		for _, row := range rows {
			lookups = append(lookups, edgeRow{otherKind: row.kind, otherID: row.id})
		}
	}
	nodes, err := resolveNodes(ctx, tagged, who, true, lookups)
	if err != nil {
		return References{}, err
	}
	members := []Member{}
	for _, row := range out {
		node, found := nodes[[2]string{row.kind, row.id}]
		switch {
		case !found:
		case node.Kind == graphstore.KindArtifact:
			members = append(members, Member{Kind: graphstore.KindArtifact, Address: node.Ref, RefKey: node.refKey, Depth: 1, Via: row.via})
		case node.Kind == graphstore.KindLens && row.movedFrom != nil:
			members = append(members, Member{Kind: graphstore.KindLens, Address: node.Ref, RefKey: *row.movedFrom, Depth: 1, Via: row.via})
		}
	}
	slices.SortFunc(members, func(left, right Member) int {
		return cmp.Or(strings.Compare(left.RefKey, right.RefKey), strings.Compare(left.Kind, right.Kind))
	})
	return References{
		Members:    members,
		Links:      related(out, nodes, serverURL),
		LinkedFrom: related(in, nodes, serverURL),
	}, nil
}

// closureArgs is what both statements of the read bind: issueKey at $1, the acting credential
// who proves at closureSeen (VisibilityArg), and the pointer kinds of the installed block schema
// at closureLinkKinds (pmdoc.LinkKinds), read as the read starts.
func closureArgs(issueKey string, who model.Actor) []any {
	return []any{issueKey, VisibilityArg(who), pmdoc.LinkKinds()}
}

// readLinkRows runs statement, the out or the in statement, with args (closureArgs), and answers
// its rows.
func readLinkRows(ctx context.Context, q Queryer, statement string, args []any) ([]linkRow, error) {
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (linkRow, error) {
		var link linkRow
		err := row.Scan(&link.kind, &link.id, &link.kinds, &link.via.Kind, &link.via.ID, &link.movedFrom)
		return link, err
	})
}

// related is one link for each node of rows that nodes names, in the order of the node's kind,
// then its id, each named as resolvedNode.name names it under serverURL. Two rows can name one
// node: a retired document's address names its issue (loadArtifactNodes), so content that links
// both the issue and that document, or two retired documents of one issue, reads as a row for each
// address, which the statement grouped by (oneRowPerNode). Such rows are one link, holding the
// kinds of both, each once and in order, and the lower of their vias, by kind and then id. It is
// never nil.
func related(rows []linkRow, nodes map[[2]string]resolvedNode, serverURL string) []model.ReferenceLink {
	list := make([]model.ReferenceLink, 0, len(rows))
	listed := make(map[[2]string]int, len(rows))
	for _, row := range rows {
		node, found := nodes[[2]string{row.kind, row.id}]
		if !found {
			continue
		}
		key := [2]string{node.Kind, node.ID}
		if at, seen := listed[key]; seen {
			link := &list[at]
			kinds := slices.Concat(link.Kinds, row.kinds)
			slices.Sort(kinds)
			link.Kinds = slices.Compact(kinds)
			if cmp.Or(strings.Compare(row.via.Kind, link.Via.Kind), strings.Compare(row.via.ID, link.Via.ID)) < 0 {
				link.Via = row.via
			}
			continue
		}
		listed[key] = len(list)
		list = append(list, model.ReferenceLink{Node: node.GraphNode, Name: node.name(serverURL), Kinds: row.kinds, Via: row.via})
	}
	slices.SortFunc(list, func(left, right model.ReferenceLink) int {
		return cmp.Or(strings.Compare(left.Node.Kind, right.Node.Kind), strings.Compare(left.Node.ID, right.Node.ID))
	})
	return list
}
