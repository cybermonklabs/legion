package refs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The nodes a piece of work is reasoned about with (spec requirements r-lens and
// r-constraint-risk): a lens, invoked on a node and logged under it, `dispatch://lens/<uuid>`;
// a finding of a lens, a block node whose id names its lens, `dispatch://lens/<uuid>#b-<id>`; and
// a risk under a pre-mortem, `dispatch://risk/<uuid>`. Each is a node of the store alone, with no
// projection row, so each is read from graph_nodes, through the predicate of its kind, which
// compares the author a credential proved, so a node reserved for its author reaches that author
// alone. One the store holds gone is absent, as a node that does not exist is.
//
// Each stands on another node through its current part_of tuple: a lens on the node it was
// invoked on - the issue, a document or a block of one, an ask, a comment or a message or a block
// of one, or another lens, a finding, a risk or a constraint (P9) - and a finding or a risk under
// its lens. So each is located by walking that tuple up to the first node that is none of these,
// and that node's issue is the node's own (StandingIssueSQL): FindNodeUnderIssue finds a lens, a
// finding or a risk under the issue its walk ends at, and under no other.

// standingKinds are the kinds of node that stand on another through their part_of tuple and no
// projection row, as the graph addresses each, sorted: the kinds standingText states the text of.
// The walk of StandingIssueSQL passes through every one of them to the node at the foot of the
// chain. It is filled at init rather than in its own declaration because the predicate of each of
// those kinds tests staleness over them (StaleSQL) and standingText reads that predicate, which as
// the variable's initializer is an initialization cycle the compiler refuses; no initializer of the
// package reads it.
var standingKinds []string

func init() { standingKinds = slices.Sorted(maps.Keys(standingText)) }

// StandingIssueSQL is the key of the issue the node whose kind and store id are the SQL kindExpr
// and idExpr stands under, as a scalar expression, null where it stands under none. kindExpr may
// be the kind the graph addresses the node by or the kind the store keys it by: a finding of a
// lens is a lens_block to one and a block to the other (graphstore.AddressKindSQL reads it).
//
// A node of a standing kind (standingKinds) stands where its current part_of tuple says, so the
// walk follows that tuple, the newest asserted one of each node, up through every node of those
// kinds - a lens on a lens, a finding under its lens, a risk under a pre-mortem - to the first
// node of any other kind, whose issue is the answer: an issue is its own, a document's is the
// issue it belongs to, a block's its document's, an ask's, a comment's or a message's the issue
// it belongs to, and an item's block its item's. A walk that meets a node a second time stops
// there, so a chain a move closed into a loop answers no issue rather than reading forever. The
// walk reads each node's own tuples and no node's visibility: where a node stands is its own
// fact, and whether a reader may see the nodes it stands on is each of those nodes' own
// predicate's to say where a read answers them.
func StandingIssueSQL(kindExpr, idExpr string) string {
	address := graphstore.AddressKindSQL(`standing.kind`, `standing.id`)
	return `(` + standingWalkSQL(`standing`, kindExpr, idExpr, nil) + `
		select ` + issueOfSQL(`standing.kind`, `standing.id`) + `
		from standing
		where ` + address + ` not in (` + quotedKinds(standingKinds) + `)
		limit 1)`
}

// walkColumn is one column the part_of walk of standingWalkSQL carries beside each node's kind and
// id: its name, its value at the node the walk starts at, and its value at each node a step
// reaches, written over the walk's row of the node the step leaves.
type walkColumn struct{ name, start, step string }

// standingWalkSQL is the recursive common table expression name (kind, id), with carried's column
// after them where carried is not nil, of the part_of walk StandingIssueSQL and StaleSQL both walk:
// it starts at the node whose kind and store id are the SQL kindExpr and idExpr, and each step reads
// the far end of the current part_of tuple of a node it holds of a standing kind
// (CurrentFarEndSQL), under the kind the store keys that node by, so it walks up through every node
// of those kinds to the first node of any other kind. Its `union` drops a row it holds already, so a
// chain a move closed into a loop ends the walk rather than reading forever.
func standingWalkSQL(name, kindExpr, idExpr string, carried *walkColumn) string {
	columns, start, step := `kind, id`, ``, ``
	if carried != nil {
		columns, start, step = columns+`, `+carried.name, `, `+carried.start, `, `+carried.step
	}
	return `with recursive ` + name + ` (` + columns + `) as (
			select (` + kindExpr + `)::text, (` + idExpr + `)::text` + start + `
			union
			select placed.to_kind, placed.to_id` + step + `
			from ` + name + `
			cross join lateral (
				` + CurrentFarEndSQL(`newest.to_kind, newest.to_id`, storedKindSQL(name+`.kind`), name+`.id`, graphstore.TuplePartOf) + `
			) placed
			where ` + graphstore.AddressKindSQL(name+`.kind`, name+`.id`) + ` in (` + quotedKinds(standingKinds) + `)
		)`
}

// StaleSQL is the one test of a stale node (spec requirement r-proposed), true of the node of a
// standing kind whose kind and store id are the SQL kindExpr and idExpr: one whose part_of walk, up
// through every node of a standing kind as StandingIssueSQL walks it (standingWalkSQL), reaches a
// stale lens, the node itself among them. A lens is stale where it records a document's row
// (graphstore.AttributeArtifactID) and no row with that id stands at the address of the node it
// stands on - the document itself, or the document whose block it is, `<ref_key>#<block id>`: that
// row is gone, or another row stands at the address. So a lens on a removed document, the findings
// under it, the risks under it and every lens on one of those are stale, whatever build removed the
// document and whatever build made a row at its address since, since a row's id never moves and no
// build gives a new row an old one's. A lens on a retired document, or on a block a version
// removed, is not stale: the row stands. A lens that records no row - one on no document, or one
// the store holds from before the rule - is not stale either, and neither is a node of any other
// kind, whose walk stops at itself.
//
// Only the lens at the foot of the walk, the one that stands on no node of a standing kind, can
// record a row, so the test is of that one: the walk carries each node's child where the child is
// a lens, and the node it ends at names the lens beneath it (stale_foot.below). Each step reads
// the node's current part_of tuple through graph_tuples_current, the lens's recorded row is one
// probe of the graph_nodes primary key, and the row at its document's address one probe of the
// artifacts table's unique index on ref_key, so a lens on a document costs two tuple probes and two
// row probes, and a node deeper down one tuple probe more for each step. Both probes are written as
// subqueries of one row each - the lens's row as a lateral subquery that ends in `offset 0`, the
// address's row as a scalar one - so the planner keeps each a probe of its key rather than a join it
// may hash from a scan of every lens or every document, as it planned the two when they were joins.
// Every read of the lens, finding, risk and constraint kinds binds it through the kind's predicate
// (VisibleKind, VisibleKindRow, VisibleEnd, VisibleNode, VisibleRow), beside the state those reads
// hold to not gone, so a stale node answers as a gone one does: 404 NOT_FOUND by its address, as an
// address nobody holds answers, and in no list, chain or count.
func StaleSQL(kindExpr, idExpr string) string {
	foot := graphstore.AddressKindSQL(`stale_foot.kind`, `stale_foot.id`)
	below := walkColumn{name: `below`, start: `null::text`,
		step: `case when stale_walk.kind = '` + graphstore.KindLens + `' then stale_walk.id end`}
	return `exists (` + standingWalkSQL(`stale_walk`, kindExpr, idExpr, &below) + `
		select 1 from stale_walk stale_foot
		cross join lateral (
			select stale_lens.attributes->>'` + graphstore.AttributeArtifactID + `' as recorded
			from graph_nodes stale_lens
			where stale_lens.kind = '` + graphstore.KindLens + `' and stale_lens.id = stale_foot.below
			offset 0
		) stale_lens
		where ` + foot + ` not in (` + quotedKinds(standingKinds) + `)
		  and stale_lens.recorded is not null
		  and (select stale_row.id::text from artifacts stale_row
		       where stale_row.ref_key = split_part(stale_foot.id, '#', 1)) is distinct from stale_lens.recorded)`
}

// staleOfKindSQL is StaleSQL over a node whose kind is the SQL kindExpr rather than a literal: run
// only where that kind, as the graph addresses it, is a standing kind, and false for every other
// node without reading anything. The planner charges the walk to every row even so, so a
// predicate over a column that can hold no standing kind binds none of it (NodeKinds).
func staleOfKindSQL(kindExpr, idExpr string) string {
	return `(case when ` + graphstore.AddressKindSQL(kindExpr, idExpr) + ` in (` + quotedKinds(standingKinds) + `)
		then ` + StaleSQL(kindExpr, idExpr) + ` else false end)`
}

// GoneOrStaleRow is whether the `graph_nodes` row a read holds aliased alias is one every read
// answers as gone: its state is gone, or it is a stale node of a standing kind (staleOfKindSQL,
// which reads nothing for a row of any other kind). The predicate of a gone row a reader may see
// binds it (VisibleGoneRow), and so does a write's test of whether it writes on a gone node (api's
// storedGone).
func GoneOrStaleRow(alias string) string {
	return `(` + alias + `.state = '` + graphstore.StateGone + `' or ` + staleOfKindSQL(alias+`.kind`, alias+`.id`) + `)`
}

// notStaleOfLiteralKind is StaleSQL negated and opened with ` and `, over the node of kind whose
// id address names, for a predicate over one kind: the test for a standing kind, read under the
// kind the store keys it by, and nothing for every other kind.
func notStaleOfLiteralKind(kind, address string) string {
	if !slices.Contains(standingKinds, kind) {
		return ""
	}
	return ` and not ` + StaleSQL(`'`+StoredKindOf(kind)+`'`, address)
}

// CurrentFarEndSQL is the read of the far end of the current tuple of kind out of the node whose
// kind and id, as the store keys them, are the SQL fromKind and fromID, for a relation that holds
// one far end at a time - a node's part_of, its authored_by - as the subquery of a lateral join:
// the newest tuple to each far end, kept while it asserts (graphstore.CurrentTupleRead), and of
// those the first by far end where the store holds several. columns is what it answers, written
// over newest: its to_kind, its to_id and its seq.
func CurrentFarEndSQL(columns, fromKind, fromID, kind string) string {
	return `select ` + columns + ` from (
					` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.seq, t.asserted`,
		`t.from_kind = `+fromKind+` and t.from_id = `+fromID+` and t.kind = '`+kind+`'
						and t.to_kind <> '`+graphstore.KindValue+`'`) + `
				) newest
				where newest.asserted
				order by newest.to_kind, newest.to_id
				limit 1`
}

// issueOfSQL is the issue key of the node of no standing kind whose kind and store id are the
// SQL kindExpr and idExpr, as every loader of those kinds answers its IssueKey: an issue is its
// own key; a document (by its ref_key, as the store keys it) belongs to its issue; an ask, a
// comment or a message to its issue; and a block, named `<owner>#<block id>`, belongs to its
// owner's issue, the item whose kind and uuid the owner spells (`comment/<uuid>`) or else the
// document whose ref_key it is, whichever block kind names it. A kind of no node that stands
// under an issue answers null.
func issueOfSQL(kindExpr, idExpr string) string {
	owner := `split_part(` + idExpr + `, '#', 1)`
	var items, blocks strings.Builder
	for _, item := range itemKinds {
		tables, _ := item.tables()
		items.WriteString(` when '` + string(item) + `' then (select owner_item.issue_key from ` + tables.items +
			` owner_item where owner_item.id::text = ` + idExpr + `)`)
		blocks.WriteString(` when ` + owner + ` like '` + string(item) + `/%' then (select owner_item.issue_key from ` + tables.items +
			` owner_item where owner_item.id::text = split_part(` + owner + `, '/', 2))`)
	}
	return `(case ` + kindExpr + `
			when '` + graphstore.KindIssue + `' then ` + idExpr + `
			when '` + graphstore.KindArtifact + `' then (select owner_document.issue_key from artifacts owner_document where owner_document.ref_key = ` + idExpr + `)` +
		items.String() + `
			else case when ` + kindExpr + ` in ` + blockNodeKinds + ` then case` + blocks.String() + `
				else (select owner_document.issue_key from artifacts owner_document where owner_document.ref_key = ` + owner + `) end end
		end)`
}

// standingText is the SQL of the text of a node of each standing kind (standingKinds), over its
// graph_nodes row aliased alias, for the reader bound at param: the text its loader answers, which
// an edge at it shows an excerpt of, and its row on a person's page shows the first line of
// (StandingTextSQL).
//
//   - A lens's is its verdict (graphstore.AttributeVerdict), and where it holds none, the words of
//     its kind (LensKindSQL), so a lens with no verdict yet still reads as the kind of analysis it
//     is; one with neither reads empty.
//   - A finding's is the newest value of its `text` tuple (graphstore.AttributeText).
//   - A risk's is its heading (graphstore.AttributeHeading).
//   - A constraint's is its standard, or its measurement where it holds no standard.
var standingText = map[string]func(alias, param string) string{
	graphstore.KindLens: func(alias, param string) string {
		return `coalesce(nullif(` + alias + `.attributes->>'` + graphstore.AttributeVerdict + `', ''), (
			` + LensKindSQL(alias, `kind_node.attributes->>'`+graphstore.AttributeWords+`'`, param) + `), '')`
	},
	graphstore.KindLensBlock: func(alias, _ string) string {
		return `coalesce((
			select case when said.asserted then said.to_id end
			from graph_tuples said
			where said.from_kind = ` + alias + `.kind and said.from_id = ` + alias + `.id
				and said.kind = '` + graphstore.AttributeText + `' and said.to_kind = '` + graphstore.KindValue + `'
			order by said.seq desc
			limit 1), '')`
	},
	graphstore.KindRisk: func(alias, _ string) string {
		return `coalesce(` + alias + `.attributes->>'` + graphstore.AttributeHeading + `', '')`
	},
	graphstore.KindConstraint: func(alias, _ string) string {
		return `coalesce(` + alias + `.attributes->>'` + graphstore.AttributeStandard + `', ` +
			alias + `.attributes->>'` + graphstore.AttributeMeasurement + `', '')`
	},
}

// LensKindSQL is the read of the kind of the lens whose graph_nodes row alias names, for the
// reader bound at param: the lens_kind node its current has_kind tuple names, where the store
// holds that node, not gone, and the reader may see it through its kind's predicate, the first by
// id where the lens names several. It answers columns, written over that node's row, kind_node,
// as a scalar subquery or a lateral join reads it. Every read of a lens's kind is this one - the
// lens read's kind of each lens, and a lens's text where it holds no verdict (standingText) - so
// no two reads give one lens two kinds, or the words of a kind that is gone.
func LensKindSQL(alias, columns, param string) string {
	return `select ` + columns + `
			from (
				` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = `+alias+`.kind and t.from_id = `+alias+`.id and t.kind = '`+graphstore.TupleHasKind+`'
					and t.to_kind <> '`+graphstore.KindValue+`'`) + `
			) chosen
			join graph_nodes kind_node on kind_node.kind = chosen.to_kind and kind_node.id = chosen.to_id
			where chosen.asserted and chosen.to_kind = '` + graphstore.KindLensKind + `' and kind_node.state <> '` + graphstore.StateGone + `'
				and ` + VisibleKindRow(graphstore.KindLensKind, "kind_node", param) + `
			order by chosen.to_id
			limit 1`
}

// StandingTextSQL is the text of the node of a standing kind whose graph_nodes row alias names,
// for the reader bound at param, whichever of those kinds it is (standingText), read off the row's
// own kind and id (graphstore.AddressKindSQL), and null for a row of any other kind: what a read
// listing nodes of several kinds at once - a person's page - shows of each.
func StandingTextSQL(alias, param string) string {
	var cases strings.Builder
	for _, kind := range standingKinds {
		cases.WriteString(` when '` + kind + `' then ` + standingText[kind](alias, param))
	}
	return `(case ` + graphstore.AddressKindSQL(alias+`.kind`, alias+`.id`) + cases.String() + ` end)`
}

// StandingKinds is every kind of node that stands on another through its part_of tuple alone, as
// the graph addresses each (standingKinds): a constraint, a lens, a finding of a lens and a risk.
func StandingKinds() []string { return slices.Clone(standingKinds) }

// StoreRef is the `dispatch://` address of a node the store addresses by its own id alone - an
// interaction, a lens, a finding of a lens, a risk, a constraint, a kind of the lens catalogue -
// from its kind, as the graph addresses it, and its store id: `dispatch://<kind>/<id>`, and for a
// finding, whose id names its lens (graphstore.ParseFindingID), its lens's address with the block
// id as a document block's reference writes one (`dispatch://lens/<uuid>#b-<id>`).
func StoreRef(kind, id string) string {
	if kind == graphstore.KindLensBlock {
		lens, block, _ := graphstore.ParseFindingID(id)
		return BlockRef(StoreRef(graphstore.KindLens, lens), block)
	}
	return "dispatch://" + kind + "/" + id
}

// standingAddressKind is the kind the graph addresses a node by whose kind and id a part_of tuple
// names, as the registry keys its loader: the kind itself, or for a block, the block kind of the
// lens or the item its id names (graphstore.AddressKind, ParseItemBlockNode), so a writer that
// named an item's block or a finding as a block still reaches the node's own loader.
func standingAddressKind(kind, id string) string {
	if kind != BlockNodeKind {
		return kind
	}
	if addressed := graphstore.AddressKind(kind, id); addressed != kind {
		return addressed
	}
	if item, _, _, ok := ParseItemBlockNode(id); ok {
		return item.BlockKind()
	}
	return kind
}

// placedNode is one node read by loadReasoningNodes, with the far end of its current part_of
// tuple, empty where it holds none, and pinned, that far end named at version, the version of its
// document or the revision of its item that last held it, where a version, a retirement or a
// revision took it out (pinnedParentSQL).
type placedNode struct {
	node                 resolvedNode
	parentKind, parentID string
	pinned               *model.NodeParent
	version              int
}

// pinnedParentSQL is the read of the node whose kind and id, as a part_of tuple names them, are
// the SQL kindExpr and idExpr, where a version, a retirement or a revision took it out of its
// issue: one row, the columns of its document that documentColumns reads, or of the item whose
// block it is (item_issue_key, item_project, item_slug, as itemRef reads them), the id of a
// document's block (block_id), and the version of the document or the revision of the item a node
// standing on it names it at (version), which pinnedParentColumns selects, and no row for any
// other node. For a node standing on it, a lens on it among them, that node is history and not a
// node taken gone, as an anchored comment on a removed paragraph keeps its place (spec requirement
// r-proposed):
//
//   - A block of a document that no current version holds (no document_blocks row) is named at
//     the last version that held it: the latest version a block-aware build indexed below the
//     version that last deleted it (indexedBelowSQL, removedAtSQL), the version its own node
//     belongs to (RemovedBlockNode), whether its document is retired since or not.
//   - A retired document (artifacts.retired_at, docs.RetireDocument), and a block of one that its
//     last version holds, are named at that last version of the document.
//   - A block of a comment or a part of an ask that no current revision of its item holds (its
//     row's position is null) is named at the revision before the one that last removed it (the
//     newest revision whose texts row records it null), the last revision that held it and the
//     one its own node belongs to (RemovedItemBlockNode). A message records no revisions and
//     removes no block, so no block of one is pinned.
//
// A current block of a document that is not retired, a document that is not retired, a current
// block of an item and a node of any other kind answer no row, and stand where their own loader
// says.
func pinnedParentSQL(kindExpr, idExpr string) string {
	block := `substr(` + idExpr + `, length(pinned_document.ref_key) + 2)`
	last := `(select max(last_version.number) from artifact_versions last_version where last_version.artifact_id = pinned_document.id)`
	var items strings.Builder
	for _, item := range revisedItemKinds() {
		tables, _ := item.tables()
		items.WriteString(`
		union all
		select null::uuid, null::text, null::text, null::text, null::text, null::text, null::boolean,
			pinned_item.issue_key, coalesce(pinned_issue.project_key, pinned_on.project_key, ''), coalesce(pinned_on.slug, ''),
			null::text, pinned_removal.revision - 1
		from ` + tables.blocks + ` pinned_item_block
		join ` + tables.items + ` pinned_item on pinned_item.id = pinned_item_block.` + tables.item + `
		left join issues pinned_issue on pinned_issue.key = pinned_item.issue_key
		left join artifacts pinned_on on pinned_on.id = pinned_item.artifact_id
		cross join lateral (
			select ` + itemRemovedAtSQL(tables, `pinned_item_block.`+tables.item, `pinned_item_block.block_id`) + ` as revision
		) pinned_removal
		where ` + kindExpr + ` in ('` + BlockNodeKind + `', '` + item.BlockKind() + `')
			and pinned_item_block.node_id = ` + idExpr + ` and pinned_item_block.position is null
			and pinned_removal.revision > 1`)
	}
	return `select pinned_document.id, pinned_document.issue_key, pinned_document.project_key, pinned_document.session_id,
			pinned_document.slug, pinned_document.kind, pinned_document.is_primary,
			null::text as item_issue_key, null::text as item_project, null::text as item_slug,
			case when ` + kindExpr + ` = '` + BlockNodeKind + `' then ` + block + ` end as block_id, pinned_at.version
		from artifacts pinned_document
		cross join lateral (
			select case
				when ` + kindExpr + ` = '` + graphstore.KindArtifact + `'
					or exists (select 1 from document_blocks held_block where held_block.node_id = ` + idExpr + `)
				then case when pinned_document.retired_at is not null then ` + last + ` end
				else ` + indexedBelowSQL("pinned_document.id", removedAtSQL("pinned_document.id", block)) + `
			end as version
		) pinned_at
		where ` + kindExpr + ` in ('` + graphstore.KindArtifact + `', '` + BlockNodeKind + `')
			and pinned_document.ref_key = case when ` + kindExpr + ` = '` + graphstore.KindArtifact + `' then ` + idExpr + `
				else split_part(` + idExpr + `, '#', 1) end
			and pinned_at.version is not null` + items.String()
}

// revisedItemKinds is each item kind whose revisions are recorded (itemTables.revisions), in
// itemKinds' order: a comment and an ask, whose revisions remove blocks, which pinnedParentSQL
// names at the revision that last held them. A message records no revisions.
func revisedItemKinds() []ItemKind {
	var revised []ItemKind
	for _, item := range itemKinds {
		if tables, _ := item.tables(); tables.revisions() {
			revised = append(revised, item)
		}
	}
	return revised
}

// PinnedKinds is every kind of node, as a part_of tuple names it, that pinnedParentSQL reads, and
// so PinnedParent can name at a version or a revision: a document and its block, and the block of
// each item kind whose revisions are recorded (revisedItemKinds). For a node of any other kind
// PinnedParent answers nil.
func PinnedKinds() []string {
	kinds := []string{graphstore.KindArtifact, BlockNodeKind}
	for _, item := range revisedItemKinds() {
		kinds = append(kinds, item.BlockKind())
	}
	return kinds
}

// pinnedParentColumns selects, from the row pinnedParentSQL reads aliased alias, what
// pinnedParent.targets scans, in its order: its document's columns (documentColumns), its item's,
// its block's id and its version. They are read off the row rather than inside the read, so a
// node the read finds no row for, which a left join answers null, scans as no document and no
// item.
func pinnedParentColumns(alias string) string {
	return documentColumns(alias) + `, ` + alias + `.item_issue_key, ` + alias + `.item_project, ` + alias + `.item_slug, ` +
		alias + `.block_id, ` + alias + `.version`
}

// pinnedParent is the node pinnedParentSQL reads, as its columns select it: its document, or the
// item whose block it is - the issue the item belongs to, its project and the slug of the document
// it hangs on, as itemRef reads them, project and slug nil for no item - the id of a document's
// block, and the version or the revision a node standing on it names it at, nil where the read
// found no row.
type pinnedParent struct {
	document                         documentParent
	itemIssue, itemProject, itemSlug *string
	block                            *string
	version                          *int
}

func (p *pinnedParent) targets() []any {
	return append(p.document.targets(), &p.itemIssue, &p.itemProject, &p.itemSlug, &p.block, &p.version)
}

// parent is the node, whose node id is nodeID, as a node standing on it names it, and the version
// it names it at: the block or the document, by its uuid or its block's node id as its own loader
// names it, or the block of an item by its node id (itemBlockParent), its address pinned to that
// version or revision (`dispatch://KEY/artifact/<slug>#b-<id>@v<N>`,
// `dispatch://KEY/artifact/<slug>@v<N>`, `dispatch://KEY/comment/<uuid>#b-<id>@v<N>`,
// `dispatch://KEY/ask/<uuid>#b-<id>@v<N>`), the one address that reads it now. It is nil where the
// read found no row.
func (p pinnedParent) parent(nodeID string) (*model.NodeParent, int) {
	if p.version == nil {
		return nil, 0
	}
	named := p.document.parent()
	switch {
	case p.itemProject != nil && p.itemSlug != nil:
		named = itemBlockParent(p.itemIssue, *p.itemProject, *p.itemSlug, nodeID)
	case named == nil:
		return nil, 0
	case p.block != nil:
		named = &model.NodeParent{Kind: BlockNodeKind, ID: nodeID, Ref: BlockRef(named.Ref, *p.block)}
	}
	if named.Ref != "" {
		named.Ref += "@v" + strconv.Itoa(*p.version)
	}
	return named, *p.version
}

// PinnedParent is the node of kind and id, as a part_of tuple names it, as a lens standing on it
// names it where a version, a retirement or a revision took it out of its issue
// (pinnedParentSQL), its address pinned to the version of its document or the revision of its
// item that last held it, and nil for any other node. The lens read names each lens on such a node
// by it, so the address a lens's on answers is the one the read of the node answers, and is the
// parent loadReasoningNodes names. It reads no predicate: its caller has read the node through
// one.
func PinnedParent(ctx context.Context, q Queryer, kind, id string) (*model.NodeParent, error) {
	var pinned pinnedParent
	err := q.QueryRow(ctx, `select `+pinnedParentColumns("pinned")+` from (`+pinnedParentSQL(`$1::text`, `$2::text`)+`) pinned`,
		kind, id).Scan(pinned.targets()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the version a lens on %s %s names it at: %w", kind, id, err)
	}
	parent, _ := pinned.parent(id)
	return parent, nil
}

// loadReasoningNodes resolves the nodes of kind - lenses, the findings of lenses (lens_block) or
// risks - whose store ids are ids, for who, in one statement over graph_nodes, under the kind the
// store keys them by (StoredKindOf): each row through its kind's predicate over the row the read
// holds (VisibleKindRow, in heldNodesWhere), with its state and visibility, its project, its text
// (standingText), the far end of its current part_of tuple, and the issue it stands under. That is
// the issue its parent stands under or belongs to, so the walk of StandingIssueSQL starts at that
// far end, the row placed has already read, rather than reading the node's part_of a second time.
// Its address is the store's (StoreRef).
//
// A reader who may see the node may see the node it stands on, so its parent is named with no
// predicate of the parent's own, and no node answers a parent that is missing because it is
// hidden. The store refuses a part_of out of a node every reader may see at a node reserved for
// its author (graphstore.VisibilityWiderThanParentError), a route places a node only under one
// its caller may see, so a node reserved for its author stands only under its author's own or
// under one every reader sees, and the release before this one wrote no node reserved for its
// author at all.
//
// A finding is stored as a block, and of the store's blocks only one whose id names its lens
// (graphstore.FindingIDSQL) is one. A finding is no block of any document, so it answers no
// Document and no version; its BlockID is its own block id, and its text is cut as a document
// block's stored excerpt is (BlockExcerptRunes), which an edge at it shows whole.
//
// Its parent is that far end, as every loader's is the node it belongs to. A block of a document
// that a version removed, a retired document and a block of one, and a block of a comment or a
// part of an ask that a revision of its item removed, are history for the node standing on them,
// and no node taken gone (spec requirement r-proposed): each is named at the version of its
// document or the revision of its item that last held it, from the columns of the same statement
// (pinnedParentSQL), with that version as the node's ParentVersion, since no loader answers a
// block no current version or revision holds, and the address of a retired document read by its
// ref_key answers its issue (loadArtifactNodes). So no lens on what a version, a retirement or a
// revision took out answers a null parent. Any other parent is named where
// the store does not hold it gone: a lens on a finding its lens took gone with it, or on a lens a
// person took gone, names no parent, as no read answers that node, while the issue it stands under
// is still the one the walk reaches through it; a person's gone takes every node under it gone
// with it (api's writeNodeState), so no such lens is live. A parent of a standing kind is named by
// its kind, its store id and its address; any other is resolved through its own kind's loader, in
// one more read for the whole batch (resolveNodes), so it carries the id and the address that
// loader answers - an artifact's uuid, a block's node id.
func loadReasoningNodes(ctx context.Context, q Queryer, who model.Actor, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select held.id, held.made_by, held.made_at, state, visibility, held.project_key, `+standingText[kind]("held", "$4")+`,
		       case when placed_parent.state_held is distinct from $3 or pinned.version is not null then placed.to_kind end,
		       case when placed_parent.state_held is distinct from $3 or pinned.version is not null then placed.to_id end,
		       `+StandingIssueSQL(`placed.to_kind`, `placed.to_id`)+`, `+pinnedParentColumns("pinned")+`
		from graph_nodes held
		left join lateral (
			`+CurrentFarEndSQL(`newest.to_kind, newest.to_id`, `held.kind`, `held.id`, graphstore.TuplePartOf)+`
		) placed on true
		left join lateral (
			select parent_row.state as state_held from graph_nodes parent_row
			where parent_row.kind = `+storedKindSQL(`placed.to_kind`)+` and parent_row.id = placed.to_id
			offset 0
		) placed_parent on true
		left join lateral (
			`+pinnedParentSQL(`placed.to_kind`, `placed.to_id`)+`
		) pinned on true
		where `+heldNodesWhere(kind),
		StoredKindOf(kind), ids, graphstore.StateGone, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	var placed []placedNode
	for rows.Next() {
		node := model.GraphNode{Kind: kind}
		var madeBy []byte
		var madeAt time.Time
		var project, parentKind, parentID *string
		var body string
		var pinned pinnedParent
		if err := rows.Scan(append([]any{&node.ID, &madeBy, &madeAt, &node.State, &node.Visibility, &project, &body,
			&parentKind, &parentID, &node.IssueKey}, pinned.targets()...)...); err != nil {
			rows.Close()
			return fmt.Errorf("scan %s node: %w", kind, err)
		}
		node.Ref = StoreRef(kind, node.ID)
		if project != nil {
			node.Project = *project
		}
		if kind == graphstore.KindLensBlock {
			_, node.BlockID, _ = graphstore.ParseFindingID(node.ID)
			body = text.HeadRunes(body, BlockExcerptRunes)
		}
		if err := setMadeAndChanged(&node, madeBy, madeAt, nil, nil, storedActors); err != nil {
			rows.Close()
			return err
		}
		one := placedNode{node: resolvedNode{GraphNode: node, text: body}}
		if parentKind != nil && parentID != nil {
			one.parentKind, one.parentID = standingAddressKind(*parentKind, *parentID), *parentID
			one.pinned, one.version = pinned.parent(*parentID)
		}
		placed = append(placed, one)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	return placeReasoningNodes(ctx, q, who, placed, nodes)
}

// heldNodesWhere is the condition a read of the nodes of kind, whose store ids are a batch, holds
// each row of graph_nodes to, the row aliased held: of the kind the store keys kind by ($1,
// StoredKindOf), at one of the ids ($2), not in the state $3 (graphstore.StateGone), and seen by
// the reader $4 (VisibilityArg) through its kind's predicate over the row (VisibleKindRow). Of
// the store's blocks only one whose id names its lens (graphstore.FindingIDSQL) is a finding.
// loadReasoningNodes binds it, and so does BlockTexts for a finding's whole text, so the text of a
// finding is read off the same rows its name is.
func heldNodesWhere(kind string) string {
	where := `held.kind = $1 and held.id = any($2) and held.state <> $3`
	if kind == graphstore.KindLensBlock {
		where += ` and ` + graphstore.FindingIDSQL("held.id")
	}
	return where + ` and ` + VisibleKindRow(kind, "held", "$4")
}

// placeReasoningNodes gives each node its parent, as loadReasoningNodes says, and adds it to nodes:
// a parent the statement named at a version, with that version, as it named it; one of a standing
// kind by its own address; and any other through its loader.
func placeReasoningNodes(ctx context.Context, q Queryer, who model.Actor, placed []placedNode, nodes map[[2]string]resolvedNode) error {
	var lookups []edgeRow
	for _, one := range placed {
		if one.parentKind != "" && one.pinned == nil && !slices.Contains(standingKinds, one.parentKind) {
			lookups = append(lookups, edgeRow{otherKind: one.parentKind, otherID: one.parentID})
		}
	}
	var parents map[[2]string]resolvedNode
	if len(lookups) > 0 {
		var err error
		if parents, err = resolveNodes(ctx, q, who, true, lookups); err != nil {
			return err
		}
	}
	for _, one := range placed {
		node := one.node
		switch {
		case one.parentKind == "":
		case one.pinned != nil:
			node.Parent, node.ParentVersion = one.pinned, &one.version
		case slices.Contains(standingKinds, one.parentKind):
			node.Parent = &model.NodeParent{Kind: one.parentKind, ID: one.parentID, Ref: StoreRef(one.parentKind, one.parentID)}
		default:
			if parent, found := parents[[2]string{one.parentKind, one.parentID}]; found {
				node.Parent = &model.NodeParent{Kind: parent.Kind, ID: parent.ID, Ref: parent.Ref}
			}
		}
		nodes[[2]string{node.Kind, node.ID}] = node
	}
	return nil
}
