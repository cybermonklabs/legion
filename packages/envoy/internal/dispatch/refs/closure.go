package refs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Member is a document of an issue's Links, one its own content links to, with that row's via.
// The read reads one hop, so Depth is always 1; the answer's members field keeps it for the
// clients that print it.
type Member struct {
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

// documentTarget is true of a link row r whose target is a document, or a block a current
// version of its document holds; documentOfTarget is that document's ref_key, which never holds
// a '#', and r's own target for any other kind. documentBlockHeld is the join that answers
// whether a block target is held, and documentTarget reads its answer rather than an `exists`
// probe of its own: under an OR the probe is no semijoin, so the planner may plan it as a hashed
// subplan and read all of document_blocks to build the hash. A lateral that stops at its first
// row is a nested loop keyed through document_blocks_node_id, the hash index on exactly node_id,
// and it runs at all only for a block target, because its own to_kind test is a one-time filter
// the planner re-reads on each loop.
const (
	documentBlockHeld = `left join lateral (
				select 1 as held from document_blocks d
				where r.to_kind = 'block' and d.node_id = r.to_id
				limit 1
			) block_target on true`
	documentTarget   = `(r.to_kind in ('artifact', 'block') and (r.to_kind = 'artifact' or block_target.held is not null))`
	documentOfTarget = `case when r.to_kind = 'block' then split_part(r.to_id, '#', 1) else r.to_id end`
)

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

// targetNode is the kind and the id of the node the target of a link row r counts as, beside
// documentBlockHeld and itemBlockHeld: a document's block is its document, by ref_key, and a
// block of an item its item; any other target is itself.
const targetNode = `coalesce(item_block.kind, case when r.to_kind = 'block' then 'artifact' else r.to_kind end),
				       coalesce(item_block.id, ` + documentOfTarget + `)`

// oneRowPerNode ends both statements of the read. far holds a row for each link: the node at its
// far end (node_kind, node_id), its kind, and its via at the near end. It leaves out the issue and
// its own content, the rows (kind, id) that own selects, read once into an array, and keeps one
// row for each other node: the distinct kinds of its links in order, and its lowest via, the
// least pair of via kind and via id as arrays compare, element by element in the database's order.
func oneRowPerNode(own string) string {
	return `
		select f.node_kind, f.node_id, array_agg(distinct f.kind order by f.kind),
		       (min(array[f.via_kind, f.via_id]))[1], (min(array[f.via_kind, f.via_id]))[2]
		from far f
		where not (row(f.node_kind, f.node_id) = any(array(` + own + `)))
		group by f.node_kind, f.node_id`
}

// linksOutQuery is the out statement of the references read of the issue whose key is $1: every
// node the issue's own content links to, read from the links of that content by their source.
// The issue's own content is its documents and its items, those of each item kind (itemKinds:
// its comments, its messages and its asks), each read by the issue's key. Its links are each
// one's own refs rows, read by refs' primary key (from_kind, from_id); every link of its
// documents' blocks, mentions and the pointers block_ref_listed lists, read from block_refs by
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
// the read of its block links alike.
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
// The far end of a link is reduced to its node (targetNode): a document's block to its document
// while a current version holds the block (documentTarget), and a block of an ask, a comment or a
// message to its item while the block is current (itemBlockHeld); a link to a block neither
// holds names nothing. A document is named by its ref_key, which a link stores. The issue and its
// own content are left out (own): the issue by its key, a document by its ref_key and every
// other source as sources names it. Its rows are one for each node, with the kinds of its links
// and its lowest via: a document of the issue by its uuid, the id its links are written under, an
// item by its uuid (oneRowPerNode).
var linksOutQuery = func() string {
	ownItems := make([]string, 0, len(itemKinds))
	sources := []string{`select 'artifact', o.id::text from own_documents o`}
	// blockLinksRead is every kind of source whose block links an arm of links reads: a
	// document's, and an item's whose own refs rows do not hold them.
	blockLinksRead := []string{`'artifact'`}
	blockLinks := []string{`select r.kind, r.to_kind, r.to_id, 'artifact', o.id::text
			from own_documents o
			cross join lateral (
				select r.kind, r.to_kind, r.to_id from block_refs r
				where r.artifact_id = o.id and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))
				offset 0
			) r`}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		own := `own_` + tables.items
		ownItems = append(ownItems, own+` as (
			select x.id from `+tables.items+` x where x.issue_key = $1
		)`)
		sources = append(sources, `select '`+string(item)+`', o.id::text from `+own+` o`)
		if item.refsHoldBlockLinks() {
			continue
		}
		blockLinksRead = append(blockLinksRead, `'`+string(item)+`'`)
		blockLinks = append(blockLinks, `select r.kind, r.to_kind, r.to_id, '`+string(item)+`', o.id::text
			from `+own+` o
			cross join lateral (
				select r.kind, r.to_kind, r.to_id from `+itemBlockLinks(item)+`
				where r.`+tables.item+` = o.id and `+itemBlockCurrent+`
				offset 0
			) r`)
	}
	return `
		with own_documents as (
			select a.id, a.ref_key from artifacts a where a.issue_key = $1
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
		far (node_kind, node_id, kind, via_kind, via_id) as (
			select ` + targetNode + `, r.kind, r.via_kind, r.via_id
			from links r
			` + documentBlockHeld + `
			` + itemBlockHeld + `
			where (` + documentTarget + ` or r.to_kind not in ('artifact', 'block'))
			  and (r.to_kind not in ` + itemBlockKinds + ` or item_block.id is not null)
		)` + oneRowPerNode(`select row(o.kind, o.id) from own o`)
}()

// linksInQuery is the in statement of the references read of the issue whose key is $1: every
// node that links to the issue or to its own content, read from the links into them by their
// target. It reads the issue's own targets once (targets): the issue node by its key, its
// documents by ref_key and their current blocks (document_blocks, by its primary key's document),
// and its asks, comments and messages by the issue's key and their current blocks
// (itemBlockCurrent). Then it reads each link table once for each kind of target, `to_kind =
// <kind> and to_id = any(<the ids of that kind>)` (keys), through the table's index of targets
// (refs_to, block_refs_to_source, ask_block_refs_to, comment_block_refs_to,
// message_block_refs_to), in a lateral that `offset 0` keeps a nested loop keyed by the kind, as
// the out statement keeps its reads: a source block of an item while it is current
// (itemBlockLinks, itemBlockCurrent), a block's pointer while block_ref_listed lists it, and an
// item's own mention of a target that a block of that item holds as that block's link alone
// (heldByABlock).
//
// Each link's source is reduced to its node: a document's block to its document and a block of
// an item to the item. A source document, or a source block's document, is named by its ref_key,
// read from artifacts by its id in a lateral that stops at its first row, as the out statement
// names a document, so one naming pass serves both. Each link's target is reduced to the node it
// counts as (targetNode), its via: the id a link to that node stores, the issue's key, a
// document's ref_key or an item's uuid. The issue and its own content are left out (targets).
// Its rows are one for each node, with the kinds of its links and its lowest via (oneRowPerNode).
var linksInQuery = func() string {
	targets := []string{
		`select 'issue', $1::text`,
		`select 'artifact', o.ref_key from own_documents o`,
		`select 'block', d.node_id from own_documents o join document_blocks d on d.artifact_id = o.id`,
	}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		targets = append(targets,
			`select '`+string(item)+`', x.id::text from `+tables.items+` x where x.issue_key = $1`,
			`select '`+item.BlockKind()+`', b.node_id
			from `+tables.items+` x join `+tables.blocks+` b on b.`+tables.item+` = x.id
			where x.issue_key = $1 and `+itemBlockCurrent)
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
				cross join lateral (select a.ref_key from artifacts a where a.id = r.artifact_id limit 1) src`,
			`(r.to_types is null or block_ref_listed(r.to_types, r.to_id))`),
	}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		links = append(links, byTarget(`'`+string(item)+`', r.`+tables.item+`::text, r.kind, r.to_kind, r.to_id`,
			itemBlockLinks(item), itemBlockCurrent))
	}
	return `
		with own_documents as (
			select a.id, a.ref_key from artifacts a where a.issue_key = $1
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
		far (node_kind, node_id, kind, via_kind, via_id) as (
			select r.node_kind, r.node_id, r.kind, ` + targetNode + `
			from links r
			` + itemBlockHeld + `
			where r.node_id is not null
		)` + oneRowPerNode(`select row(t.kind, t.id) from targets t`)
}()

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
// addresses it with byRefKey (a document by its ref_key), the kinds of its links and its via.
type linkRow struct {
	kind, id string
	kinds    []string
	via      model.ReferenceVia
}

// Closure reads the references of the issue whose key is issueKey, one hop in each direction:
// Links, every node the issue's own content links to (linksOutQuery), and LinkedFrom, every node
// that links to the issue or to its own content (linksInQuery). It sends the out statement, then
// the in statement, then names the nodes of both through resolveNodes, the resolver the graph
// read names its nodes with, at most one statement for each kind of node. A row whose node
// resolveNodes does not name, a node gone since or of a kind this build does not know, is left
// out, as Edges leaves such an edge out. Each list is in the order of the node's kind, then its
// id. Members are the documents of Links, each at depth 1 with its row's via, by ref_key. Every
// statement it sends, through q, starts with ClosureTag.
func Closure(ctx context.Context, q Queryer, issueKey string) (References, error) {
	tagged := taggedQueryer{q}
	out, err := readLinkRows(ctx, tagged, linksOutQuery, issueKey)
	if err != nil {
		return References{}, fmt.Errorf("read the links out of %s's own content: %w", issueKey, err)
	}
	in, err := readLinkRows(ctx, tagged, linksInQuery, issueKey)
	if err != nil {
		return References{}, fmt.Errorf("read the links into %s and its own content: %w", issueKey, err)
	}
	lookups := make([]edgeRow, 0, len(out)+len(in))
	for _, rows := range [...][]linkRow{out, in} {
		for _, row := range rows {
			lookups = append(lookups, edgeRow{otherKind: row.kind, otherID: row.id})
		}
	}
	nodes, err := resolveNodes(ctx, tagged, true, lookups)
	if err != nil {
		return References{}, err
	}
	members := []Member{}
	for _, row := range out {
		if node, found := nodes[[2]string{row.kind, row.id}]; found && row.kind == "artifact" {
			members = append(members, Member{RefKey: node.refKey, Depth: 1, Via: row.via})
		}
	}
	slices.SortFunc(members, func(left, right Member) int { return strings.Compare(left.RefKey, right.RefKey) })
	return References{Members: members, Links: related(out, nodes), LinkedFrom: related(in, nodes)}, nil
}

// readLinkRows runs statement, the out or the in statement, for issueKey and answers its rows.
func readLinkRows(ctx context.Context, q Queryer, statement, issueKey string) ([]linkRow, error) {
	rows, err := q.Query(ctx, statement, issueKey)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (linkRow, error) {
		var link linkRow
		err := row.Scan(&link.kind, &link.id, &link.kinds, &link.via.Kind, &link.via.ID)
		return link, err
	})
}

// related is each of rows whose node nodes names, in the order of the node's kind, then its id.
// It is never nil.
func related(rows []linkRow, nodes map[[2]string]resolvedNode) []model.ReferenceLink {
	list := make([]model.ReferenceLink, 0, len(rows))
	for _, row := range rows {
		node, found := nodes[[2]string{row.kind, row.id}]
		if !found {
			continue
		}
		list = append(list, model.ReferenceLink{Node: node.GraphNode, Name: node.name(), Kinds: row.kinds, Via: row.via})
	}
	slices.SortFunc(list, func(left, right model.ReferenceLink) int {
		return cmp.Or(strings.Compare(left.Node.Kind, right.Node.Kind), strings.Compare(left.Node.ID, right.Node.ID))
	})
	return list
}
