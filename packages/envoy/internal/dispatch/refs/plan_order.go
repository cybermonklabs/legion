package refs

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// IssueHalf is one half of an issue in the order work runs in: its start, or, with Done, its
// done. An issue starts after its parent starts and after each issue it needs is done, and it is
// done after it starts and after each of its children is done. Splitting the two halves lets a
// sibling's wait on a sibling stand while still refusing a child that waits on its parent, or a
// parent that waits on one of its descendants. This is the one statement of the split: api's
// dependency-cycle refusal walks it over the rows an issue's relations project, and PlanOrder
// orders an issue's whole tree by it.
type IssueHalf struct {
	Key  string
	Done bool
}

// IssueWaits is what the halves of one issue wait through: its parent, "" for none, the issues
// it needs, and its children.
type IssueWaits struct {
	Parent   string
	Needs    []string
	Children []string
}

// Halves is h first, and for a done half the start of its issue after it: a done waits on its
// own start, so a walk that reaches the done stands at the start too.
func (h IssueHalf) Halves() []IssueHalf {
	if !h.Done {
		return []IssueHalf{h}
	}
	return []IssueHalf{h, {Key: h.Key}}
}

// Waits is every half h waits on through its issue's waits, beside the start a done waits on
// (Halves): a start waits on its parent's start and on the done half of each issue it needs, and
// a done on the done half of each child.
func (h IssueHalf) Waits(waits IssueWaits) []IssueHalf {
	if h.Done {
		held := make([]IssueHalf, len(waits.Children))
		for index, child := range waits.Children {
			held[index] = IssueHalf{Key: child, Done: true}
		}
		return held
	}
	held := make([]IssueHalf, 0, len(waits.Needs)+1)
	if waits.Parent != "" {
		held = append(held, IssueHalf{Key: waits.Parent})
	}
	for _, target := range waits.Needs {
		held = append(held, IssueHalf{Key: target, Done: true})
	}
	return held
}

// The two halves an issue stands in the ordered read as (OrderNode.Half).
const (
	HalfStart = "start"
	HalfDone  = "done"
)

// OrderNode names one node of the ordered read (PlanOrder): an issue's start or done half, Kind
// graphstore.KindIssue, ID its key and Half HalfStart or HalfDone; or a plan node, Kind
// BlockNodeKind, ID its store address and no Half. A far end outside the tree is named as its
// needs tuple names it, an issue by its done half, the half a wait on an issue waits on.
type OrderNode struct {
	Kind, ID, Half string
}

// OrderedNode is one node of the tree the ordered read answers: its name; the issue it stands
// under, an issue half's own key and a plan node's the first issue its part_of chain reaches; its
// size, the whole number of minutes a plan node holds (AttributePlanSize) written in decimal, ""
// for an issue half and for a plan node whose author gave none; the skills and the docs a plan
// node's implementer should read (AttributePlanSkills, AttributePlanDocs), each in the order its
// author wrote them, and an empty list, never nil, for a plan node that holds none and for an
// issue half; and every node it waits on, sorted (compareOrderNodes), the fixed nodes among them,
// and no overtaken one.
type OrderedNode struct {
	OrderNode
	Issue        string
	Size         string
	Skills, Docs []string
	Needs        []OrderNode
}

// TreeOrder is the order the whole tree of one issue runs in (PlanOrder).
type TreeOrder struct {
	// Order is every node of the tree in no cycle and not overtaken, Order[i] at position i, each
	// after every node it needs that has a position.
	Order []OrderedNode
	// Cycles is each set of nodes of the tree that wait on one another, its nodes sorted and the
	// sets by their first node: a strongly connected component of more than one node, or one node
	// that needs itself. No node of one has a position, and a node that waits on one keeps its own.
	Cycles [][]OrderedNode
	// Fixed is each far end outside the tree that a node of it waits on, sorted, with no position:
	// an issue's done half, a plan node of an issue above the tree.
	Fixed []OrderNode
	// Overtaken is each node of the tree whose state is overtaken and each node under one, sorted,
	// with no position; no node waits on one.
	Overtaken []OrderedNode
}

// planOrderDepth bounds the walk down an issue's part_of tuples to its plan nodes, as the climb
// up a block's chain is bounded (authoredChainDepth): a tree of an issue is a few levels deep, so
// a real chain is far shorter, and a chain a move closed into a loop ends there.
const planOrderDepth = authoredChainDepth

// PlanOrder is the order the whole tree of issue issueKey runs in, for who: the issue and every
// issue under it (IssueDescendants), each placed as its two halves (IssueHalf), with its parent
// read off its row and the issues it needs off its needs tuples; and every plan node under each
// of them - a block node not gone whose document_blocks row is a plan_task, a block of the
// issue's room keyed by its tree (`<KEY>/tree#<id>`) as much as one of a document - whose issue
// is the first one its chain in the tree reaches, whatever its address spells: up its part_of,
// and from the issue's conversation up the item it stands in, a block to its ask, comment or
// message, and an item to the item it answers or to its issue (planHangs). A plan node waits on
// its issue's start, and its issue's done waits on it; a needs tuple out of a plan node waits on
// the plan node it names, or on the done half of the issue it names. A node the tree does not
// hold is a fixed node. An issue the reader may not see, and every node under it, is no node of
// the tree, and a node that waits on one waits on nothing there; an issueKey the reader may not
// see answers nothing.
//
// It reads the tree in three statements beside IssueDescendants', however large: whether a mark
// names each issue of the tree whole; one walk (readPlanTree) down the tree from each issue's
// node, through every node who may see and never into an issue, which answers each issue's
// parent, its node's current rank and when its node was made, and each plan node with its issue,
// its current text_rank, size, skills and docs values, which the block index states from the
// node's place in its document and its opening line (planBlockValues), and whether it or a node
// above it is overtaken; and one read of every current needs tuple out of those nodes
// (graph_tuples_current).
//
// It finds the cycles by Tarjan's strongly connected components, iteratively, so a long chain
// costs no depth of the stack: every component of more than one node, or of one node that needs
// itself, is a cycle, and every other node comes back once, after every node it needs that has a
// position. The position is a needs-first depth-first walk, which keeps each chain of steps
// together (R11, waitGraph.order): it starts from the nodes that no node needs, in the base
// order, and before it places a node it places that node's unplaced needs the same way, so each
// need stands just before the first node that needs it. For plan nodes written a, b, c, where c
// needs a, the order is b, a, c. The base order (baseOrder) is the issue's start, its own plan
// nodes in their document's order - by text_rank, and by address on a tie (comparePlans) - then
// each issue under it in the tree's order, the order the tree read gives an issue's children
// (compareIssues), each the same way, and last the issue's done. The halves stand so because a
// start opens its issue and a done closes it: every plan node and every child of an issue waits
// on its start, and its done waits on each of them (IssueHalf). The waits of a done do not count
// as needs: the walk reaches a done at its place in the base order, or earlier through a node
// that needs its issue, and then places first the nodes it waits on that no node needs, so the
// plan nodes of an issue keep the order the issue's own read gives them. A node whose state is
// overtaken - a block, an ask, a comment or a message among them - every node under it on its
// chain in the tree, and an overtaken issue with every issue and node under it get no position
// (R7: such a node counts as closed with its subtree).
func PlanOrder(ctx context.Context, q Queryer, who model.Actor, issueKey string) (TreeOrder, error) {
	return planOrder(ctx, q, who, issueKey, nil)
}

// PlanOrderOfRows is PlanOrder for a read that holds heads, the head rows of issueKey that a view
// whose Types hold plan_task reads (HeadViewRows): every row of issueKey whose block_type is
// plan_task is among them, since a view reads each of the issue's own blocks a typed block of its
// types may be and every block past those whole (HeadView). So the climb over the head projection
// (planTreeHeads) starts, for issueKey's own documents, from the blocks of heads alone
// (planSeedsOfRows), and reads the blocks of the documents of the issues under it as PlanOrder
// does: never every block of issueKey's documents for its type, which the planner read in a
// sequential scan of document_blocks, the 52,266 blocks of the store, for each plan view of an
// issue of the largest plan (12 of the statement's 34 ms).
func PlanOrderOfRows(ctx context.Context, q Queryer, who model.Actor, issueKey string, heads *HeadTree) (TreeOrder, error) {
	blocks := make([]string, 0, len(heads.Rows))
	for node := range heads.Rows {
		if node.Kind == BlockNodeKind {
			blocks = append(blocks, node.ID)
		}
	}
	return planOrder(ctx, q, who, issueKey, blocks)
}

// planOrder is PlanOrder, whose climb over the head projection starts, for issueKey's own
// documents, from the blocks blocks names where it is not nil (PlanOrderOfRows).
func planOrder(ctx context.Context, q Queryer, who model.Actor, issueKey string, blocks []string) (TreeOrder, error) {
	if issueKey == "" {
		return TreeOrder{}, nil
	}
	descendants, err := IssueDescendants(ctx, q, who, issueKey)
	if err != nil {
		return TreeOrder{}, err
	}
	tree, err := readPlanTree(ctx, q, who, issueKey, descendants, blocks)
	if err != nil || tree.issues[issueKey] == nil {
		return TreeOrder{}, err
	}
	if err := tree.readNeeds(ctx, q, who); err != nil {
		return TreeOrder{}, err
	}
	return tree.order(), nil
}

// planTree is what the ordered read holds of one issue's tree before it orders it: each issue by
// its key and each plan node by its store address.
type planTree struct {
	root   string
	issues map[string]*planIssue
	plans  map[string]*planNode
}

// planIssue is one issue of the tree: its parent ("" for the root), whether its node is
// overtaken, its node's current rank and whether it holds one, when its node was made (zero where
// the store holds no node of it), its children in the tree's order (compareIssues), its plan nodes
// in their document's order (comparePlans), and the far ends of its needs tuples.
type planIssue struct {
	parent    string
	overtaken bool
	rank      string
	ranked    bool
	madeAt    time.Time
	children  []string
	plans     []string
	needs     []Target
}

// planNode is one plan node of the tree: the issue it stands under, its current text_rank ("" for
// none), its size, its skills and its docs (OrderedNode), whether it or a node above it is
// overtaken, and the far ends of its needs tuples.
type planNode struct {
	issue        string
	textRank     string
	size         string
	skills, docs []string
	overtaken    bool
	needs        []Target
}

// planHang is one way the tree hangs a conversation item that no part_of places (docs'
// placeItems, commentReplyParent, loadMessageNodes): an item of kind item, a row of table, stands
// under the node of kind parent its column names - an id, or an issue's key - where each of unset
// is null. So a comment stands under the comment it answers, else the comment its reply_to names,
// else the ask it answers, else its issue; a message under the message it answers, else its
// issue; an ask under its issue.
type planHang struct {
	item, parent  ItemKind
	table, column string
	unset         []string
}

// planHangs is every planHang, each item's in the order its parent is taken.
var planHangs = []planHang{
	{item: ItemComment, parent: ItemComment, table: "comments", column: "answers"},
	{item: ItemComment, parent: ItemComment, table: "comments", column: "reply_to", unset: []string{"answers"}},
	{item: ItemComment, parent: ItemAsk, table: "comments", column: "ask_id", unset: []string{"answers", "reply_to"}},
	{item: ItemComment, parent: graphstore.KindIssue, table: "comments", column: "issue_key", unset: []string{"answers", "reply_to", "ask_id"}},
	{item: ItemMessage, parent: ItemMessage, table: "messages", column: "in_reply_to"},
	{item: ItemMessage, parent: graphstore.KindIssue, table: "messages", column: "issue_key", unset: []string{"in_reply_to"}},
	{item: ItemAsk, parent: graphstore.KindIssue, table: "asks", column: "issue_key"},
}

// named is the hang's column of the row item as text, as a node's id spells it.
func (hang planHang) named() string {
	if hang.column == "issue_key" {
		return `item.issue_key`
	}
	return `item.` + hang.column + `::text`
}

// unsetSQL is the test that each column of the hang's unset is null on the row item.
func (hang planHang) unsetSQL() string {
	tests := make([]string, 0, len(hang.unset))
	for _, column := range hang.unset {
		tests = append(tests, ` and item.`+column+` is null`)
	}
	return strings.Join(tests, "")
}

// planWalkChildren is the body of readPlanTree's lateral read of the children the tree holds under
// the node below of its walk of the tuples, each with whether it is a conversation node the
// conversation places (conversed), which the reader's predicate does not test (every reader sees
// each ask, comment and message: graphstore.AdmitAuthorOnly): each node whose current part_of
// names below - of its part_of tuples into below, the newest by time and then seq, kept while it
// asserts, as the head projection holds a node's relations (graphstore.HeadRowsSQL) - and, of the
// conversation, each current block (itemBlockCurrent) of an item, and each item that hangs under
// below (planHangs) and holds no current part_of of its own, which places it there instead.
func planWalkChildren() string {
	arms := []string{`select placed.from_kind, placed.from_id, false as conversed from (
				select distinct on (t.from_kind, t.from_id) t.from_kind, t.from_id, t.asserted
				from graph_tuples t
				where t.to_kind = below.kind and t.to_id = below.id and t.kind = '` + graphstore.TuplePartOf + `'
				  and t.to_kind <> '` + graphstore.KindValue + `'
				order by t.from_kind, t.from_id, t.at desc, t.seq desc
			) placed
			where placed.asserted`}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		arms = append(arms, `select '`+item.BlockKind()+`'::text, b.node_id, true from `+tables.blocks+` b
			where below.kind = '`+string(item)+`' and b.`+tables.item+`::text = below.id and `+itemBlockCurrent)
	}
	for _, hang := range planHangs {
		arms = append(arms, `select '`+string(hang.item)+`'::text, item.id::text, true from `+hang.table+` item
			where below.kind = '`+string(hang.parent)+`' and `+hang.named()+` = below.id`+hang.unsetSQL()+`
			  and not exists (
				select 1 from (
					select distinct on (t.to_kind, t.to_id) t.asserted
					from graph_tuples t
					where t.from_kind = '`+string(hang.item)+`' and t.from_id = item.id::text and t.kind = '`+graphstore.TuplePartOf+`'
					  and t.to_kind <> '`+graphstore.KindValue+`'
					order by t.to_kind, t.to_id, t.at desc, t.seq desc
				) placed
				where placed.asserted)`)
	}
	return strings.Join(arms, `
			union all
			`)
}

// planHeadParents is the body of readPlanTree's lateral read, over the head projection, of each
// node the node of kind kind and id id stands under in the tree, with whether it is a conversation
// node the conversation places (conversed), as planWalkChildren finds it there: the far end of each
// part_of its head row, row, states now (graphstore.HeadRowsSQL); for a current block of an item,
// the item; and for an item whose row states no part_of, the node it hangs under (planHangs). A
// node that holds no head row (row null) states no part_of.
func planHeadParents(kind, id, row string) string {
	stored := id + ` collate "default"`
	// placed is the part_of relations a head row's relation arrays, columns, name (names).
	placed := func(columns, names string) string {
		return `from unnest(` + columns + `) stated (` + names + `)
				where stated.kind = '` + graphstore.TuplePartOf + `' and stated.to_kind <> '` + graphstore.KindValue + `'`
	}
	arms := []string{`select stated.to_kind, stated.to_id, false as conversed
				` + placed(row+`.relation_kinds, `+row+`.relation_to_kinds, `+row+`.relation_to_ids`, `kind, to_kind, to_id`)}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		arms = append(arms, `select '`+string(item)+`'::text, b.`+tables.item+`::text, true from `+tables.blocks+` b
				where `+kind+` = '`+item.BlockKind()+`' and b.node_id = `+stored+` and `+itemBlockCurrent)
	}
	for _, hang := range planHangs {
		arms = append(arms, `select '`+string(hang.parent)+`'::text, `+hang.named()+`, true from `+hang.table+` item
				where `+kind+` = '`+string(hang.item)+`' and item.id::text = `+stored+` and item.`+hang.column+` is not null`+hang.unsetSQL()+`
				  and not exists (select 1 `+placed(row+`.relation_kinds, `+row+`.relation_to_kinds`, `kind, to_kind`)+`)`)
	}
	return strings.Join(arms, `
				union all
				`)
}

// planIssueStarts is the first arm of both of readPlanTree's walks: each issue of $1 who, at $3,
// may see, as its node, with its parent, its node's state and when its node was made.
func planIssueStarts() string {
	return `select i.key, i.parent_key, '` + graphstore.KindIssue + `'::text, i.key, 0, n.state,
			       coalesce(n.state = '` + graphstore.StateOvertaken + `', false), n.made_at`
}

// planIssueRank is the body of readPlanTree's lateral read of the rank of the issue found's node:
// the newest rank value tuple out of it, by time and then seq, where it asserts.
const planIssueRank = `select case when t.asserted then t.to_id end as held
			from graph_tuples t
			where t.from_kind = '` + graphstore.KindIssue + `' and t.from_id = found.id and t.kind = '` + AttributeRank + `'
			  and t.to_kind = '` + graphstore.KindValue + `'
			order by t.at desc, t.seq desc
			limit 1`

// planAnswer is the answer of both of readPlanTree's statements over their walk, below, and plans,
// their select of the rows of its plan nodes from below as found: each issue's node, with its rank
// (planIssueRank), and each plan node, with its text_rank, size, skills and docs, as one row each,
// ordered by node, then depth, then issue.
func planAnswer(plans string) string {
	return `
		select answered.issue, answered.parent, answered.id, answered.root, answered.overtaken, answered.made,
		       answered.ranked, answered.sized, answered.skilled, answered.documented
		from (
			select found.issue, found.parent, found.id, true, found.overtaken, found.made, ranked.held, null::text, null::text, null::text, found.depth
			from below found
			left join lateral (` + planIssueRank + `) ranked on true
			where found.depth = 0
			union all
			` + plans + `
		) answered (issue, parent, id, root, overtaken, made, ranked, sized, skilled, documented, depth)
		order by answered.id, answered.depth, answered.issue`
}

// planTreeWalk is readPlanTree's statement over the tuples. Its walk, below, starts at each issue's
// node and takes, level by level, the children each node holds in the tree (planWalkChildren),
// every node that holds a part_of through who's predicate over its graph_nodes row
// (VisibleNodeOfRow), never into an issue and at most $2 levels down, each a node overtaken where
// its state or the state of a node above it on the walk is. It answers each issue's node with its
// rank, and each plan node - a block not gone whose document_blocks row is a plan_task - with its
// text_rank, size, skills and docs, each the newest value tuple of its attribute by time and then
// seq where it asserts, ordered by node, depth and issue.
func planTreeWalk() string {
	return `
		with recursive below (issue, parent, kind, id, depth, state, overtaken, made) as (
			` + planIssueStarts() + `
			from issues i
			left join graph_nodes n on n.kind = '` + graphstore.KindIssue + `' and n.id = i.key
			where i.key = any($1::text[]) and ` + VisibleKind(graphstore.KindIssue, "i.key", "$3") + `
			union all
			select below.issue, null::text, held.from_kind, held.from_id, below.depth + 1, n.state,
			       below.overtaken or coalesce(n.state = '` + graphstore.StateOvertaken + `', false), null::timestamptz
			from below
			cross join lateral (
			` + planWalkChildren() + `
			) held (from_kind, from_id, conversed)
			left join graph_nodes n on n.kind = held.from_kind and n.id = held.from_id
			where held.from_kind <> '` + graphstore.KindIssue + `' and below.depth < $2
			  and (held.conversed or ` + VisibleNodeOfRow("held.from_kind", "held.from_id", "n", "$3") + `)
		)
	` + planAnswer(`select found.issue, found.parent, found.id, false, found.overtaken, found.made,
			       valued.ranked, valued.sized, valued.skilled, valued.documented, found.depth
			from below found
			cross join lateral (
				select max(newest.to_id) filter (where newest.asserted and newest.kind = '`+AttributeTextRank+`') as ranked,
				       max(newest.to_id) filter (where newest.asserted and newest.kind = '`+AttributePlanSize+`') as sized,
				       max(newest.to_id) filter (where newest.asserted and newest.kind = '`+AttributePlanSkills+`') as skilled,
				       max(newest.to_id) filter (where newest.asserted and newest.kind = '`+AttributePlanDocs+`') as documented
				from (
					select distinct on (t.kind) t.kind, t.to_id, t.asserted from graph_tuples t
					where t.from_kind = found.kind and t.from_id = found.id and t.to_kind = '`+graphstore.KindValue+`'
					  and t.kind in ('`+AttributeTextRank+`', '`+AttributePlanSize+`', '`+AttributePlanSkills+`', '`+AttributePlanDocs+`')
					order by t.kind, t.at desc, t.seq desc
				) newest
			) valued
			where found.depth > 0 and found.kind = '`+BlockNodeKind+`' and coalesce(found.state, '`+graphstore.StateLive+`') <> '`+graphstore.StateGone+`'
			  and exists (select 1 from document_blocks d where d.node_id = found.id and d.type = '`+planTaskType+`')`)
}

// planTreeHeads is readPlanTree's statement over the head projection, for a tree whose every issue
// a mark names whole (issue_head_marks), which answers what planTreeWalk answers there. It reads
// only where a plan node stands: instead of walking down from each issue, paths climbs from each
// plan node up each node it stands under (planHeadParents), one level a step, until the path
// reaches an issue's node. Every path down from an issue to a plan node that planTreeWalk's walk
// takes is such a climb read the other way: each node on it stands under the next, through an edge
// planHeadParents reads off the node's row and the conversation's tables. So a path holds the same
// test as a step of the walk down - the node it climbs from passes who's predicate over its row
// (VisibleNodeOfRow) unless the conversation places it, and its state joins the path's overtaken -
// the same bound of $2 levels, and reaches the same depth under the same issue (below), and the
// read costs the plan's nodes and the nodes above them alone, so a tree of 45,000 blocks of other
// documents or of 9,252 comments reads its plan's few. Each node's state, its graph_nodes row's
// columns who's predicate reads (its kind and id as graph_nodes keys them, its visibility and its
// maker) and its relations, and a plan node's values, are its head row's. A path carries the head
// row of the node it stands at (node_row), read once: a start's by its seed, every other node's
// by the step that climbs to it, under any issue of the tree (a node's row says the same under
// each issue that holds it), and none for an issue's node, where a path ends. A step that read the
// row of the node it climbs from read each start's row twice, the 1,393 of an issue of the largest
// plan (4.6 of its 23 ms).
//
// The plan nodes the climb starts from (seeds, planSeeds or planSeedsOfRows) are the plan_task rows
// of the documents of the tree's issues, each whose head row, under its own issue, names it a
// plan_task: every plan node the walk reaches is one, since a block of an issue stands in the tree
// of that issue or of an issue above it (R7: a move refuses a parent outside the issue's tree and a
// node of an issue above under a node of an issue below), and the tree's issues are an issue and
// every issue under it. The rows of the issues themselves are no index of a block's type: a filter
// of them by block_type reads every row of the issue, 45,000 for an issue of the most blocks, and
// the planner, which takes an issue's rows for its share of the table, read the whole table for an
// issue of 9,252 comments and three plan tasks.
func planTreeHeads(seeds string) string {
	value := func(attribute string) string {
		return `path.value_texts[array_position(path.value_kinds, '` + attribute + `'::text)]`
	}
	return `
		with recursive below (issue, parent, kind, id, depth, state, overtaken, made) as (
			` + planIssueStarts() + `
			from issues i
			left join graph_nodes n on n.kind = '` + graphstore.KindIssue + `' and n.id = i.key
			where i.key = any($1::text[]) and ` + VisibleKind(graphstore.KindIssue, "i.key", "$3") + `
		), paths (plan, kind, id, depth, overtaken, state, value_kinds, value_texts, node_row) as (
			` + seeds + `
			union all
			select path.plan, up.kind, up.id, path.depth + 1, path.overtaken or coalesce(node.state = '` + graphstore.StateOvertaken + `', false),
			       path.state, path.value_kinds, path.value_texts, next.held
			from paths path
			cross join lateral (select (path.node_row).node_kind as kind, (path.node_row).node_address as id, (path.node_row).*) node
			cross join lateral (
				` + planHeadParents("path.kind", "path.id", "node") + `
			) up (kind, id, conversed)
			left join lateral (
				select held from issue_head_nodes held
				where up.kind <> '` + graphstore.KindIssue + `' and held.issue_key = any($1::text[])
				  and held.node_kind = up.kind collate "C" and held.node_address = up.id collate "C"
				order by held.issue_key
				limit 1) next on true
			where path.kind <> '` + graphstore.KindIssue + `' and path.depth < $2
			  and (up.conversed or ` + VisibleNodeOfRow("path.kind", "path.id", "node", "$3") + `)
		)
	` + planAnswer(`select issue.issue, null::text, path.plan, false, issue.overtaken or path.overtaken, null::timestamptz,
			       `+value(AttributeTextRank)+`, `+value(AttributePlanSize)+`, `+value(AttributePlanSkills)+`, `+value(AttributePlanDocs)+`, path.depth
			from paths path
			join below issue on issue.kind = '`+graphstore.KindIssue+`' and issue.id = path.id
			where path.kind = '`+graphstore.KindIssue+`' and coalesce(path.state, '`+graphstore.StateLive+`') <> '`+graphstore.StateGone+`'`)
}

// planSeedColumns is a start of planTreeHeads' climb: the plan node of the head row seed, at depth
// 0, with its state and values, and the row itself.
const planSeedColumns = `seed.node_address collate "default", seed.node_kind collate "default", seed.node_address collate "default", 0, false,
			       seed.state, seed.value_kinds, seed.value_texts, seed.held`

// planSeedRow is the read of the head row, under the issue issue, of the block address, where that
// row names it a plan_task.
func planSeedRow(issue, address string) string {
	return `select held.node_kind, held.node_address, held.state, held.value_kinds, held.value_texts, held from issue_head_nodes held
				where held.issue_key = ` + issue + ` and held.node_kind = '` + BlockNodeKind + `' and held.node_address = ` + address + ` collate "C"
				  and held.block_type = '` + planTaskType + `'
				limit 1`
}

// planSeeds is the starts of planTreeHeads' climb for PlanOrder: every plan_task block of the
// documents of the issues of $1 (document_blocks, read through each document's key).
var planSeeds = `select ` + planSeedColumns + `
			from artifacts document
			join document_blocks block on block.artifact_id = document.id
			cross join lateral (` + planSeedRow("document.issue_key", "block.node_id") + `) seed
			where document.issue_key = any($1::text[]) and block.type = '` + planTaskType + `'`

// planSeedsOfRows is planSeeds for PlanOrderOfRows, the same starts read two ways: of the
// documents of the issues under the root, $1 past its first, every plan_task block, each
// document's blocks read through its key behind an `offset 0`, so that a tree with no issue under
// its root reads no block; and of the root's own documents, of the blocks $4 names alone, each
// that is a plan_task block of one of them (document_blocks_node_id) and whose head row under the
// root names it a plan_task. Each probe of a block of $4 is a lateral read with a limit, which the
// planner keeps a probe of its index and never turns into a join over every row of
// document_blocks.
var planSeedsOfRows = `select ` + planSeedColumns + `
			from artifacts document
			cross join lateral (
				select block.node_id from document_blocks block
				where block.artifact_id = document.id and block.type = '` + planTaskType + `'
				offset 0) block
			cross join lateral (` + planSeedRow("document.issue_key", "block.node_id") + `) seed
			where document.issue_key = any(($1::text[])[2:])
			union all
			select ` + planSeedColumns + `
			from unnest($4::text[]) named (address)
			cross join lateral (` + planSeedRow("($1::text[])[1]", "named.address") + `) seed
			cross join lateral (
				select from document_blocks block
				join artifacts document on document.id = block.artifact_id
				where block.node_id = named.address and block.type = '` + planTaskType + `' and document.issue_key = ($1::text[])[1]
				limit 1) typed`

// readPlanTree reads the tree of root, whose descendants are descendants, in one statement, which
// answers a row per issue and a row per plan node (PlanOrder): over the head projection where a
// mark names each issue of the tree whole (planTreeHeads), from the starts planSeedsOfRows reads
// where blocks is not nil (PlanOrderOfRows) and from planSeeds' otherwise, and over the tuples
// otherwise (planTreeWalk). A tree with an issue no mark names walks the tuples whole: the rows of
// a marked issue do not hold a node of an issue under it that stands under a node of that issue
// below, which that issue's rows hold.
func readPlanTree(ctx context.Context, q Queryer, who model.Actor, root string, descendants, blocks []string) (*planTree, error) {
	issues := append([]string{root}, descendants...)
	var marked int
	if err := q.QueryRow(ctx, `select count(*) from issue_head_marks mark where mark.issue_key = any($1::text[])`, issues).Scan(&marked); err != nil {
		return nil, fmt.Errorf("read the head marks of the tree of %s: %w", root, err)
	}
	statement, args := planTreeWalk(), []any{issues, planOrderDepth, VisibilityArg(who)}
	switch {
	case marked != len(issues):
	case blocks != nil:
		statement, args = planTreeHeads(planSeedsOfRows), append(args, blocks)
	default:
		statement = planTreeHeads(planSeeds)
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read the plan nodes of the tree of %s: %w", root, err)
	}
	tree := &planTree{root: root, issues: map[string]*planIssue{}, plans: map[string]*planNode{}}
	var issue, id string
	var parent, rank, size, skills, docs *string
	var made *time.Time
	var isIssue, overtaken bool
	if _, err := pgx.ForEachRow(rows, []any{&issue, &parent, &id, &isIssue, &overtaken, &made, &rank, &size, &skills, &docs}, func() error {
		switch {
		case isIssue:
			held := &planIssue{overtaken: overtaken}
			if parent != nil && id != root {
				held.parent = *parent
			}
			if rank != nil {
				held.rank, held.ranked = *rank, true
			}
			if made != nil {
				held.madeAt = *made
			}
			tree.issues[id] = held
		case tree.plans[id] == nil:
			// A node a raced move left under two parents is under the nearer of them, then the
			// first issue by key: the rows come in that order.
			held := &planNode{issue: issue, overtaken: overtaken}
			if rank != nil {
				held.textRank = *rank
			}
			if size != nil {
				held.size = *size
			}
			var err error
			if held.skills, err = planList(id, AttributePlanSkills, skills); err != nil {
				return err
			}
			if held.docs, err = planList(id, AttributePlanDocs, docs); err != nil {
				return err
			}
			tree.plans[id] = held
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the plan nodes of the tree of %s: %w", root, err)
	}
	for _, key := range slices.SortedFunc(maps.Keys(tree.issues), tree.compareIssues) {
		if parent := tree.issues[tree.issues[key].parent]; parent != nil {
			parent.children = append(parent.children, key)
		}
	}
	for _, address := range slices.SortedFunc(maps.Keys(tree.plans), tree.comparePlans) {
		if issue := tree.issues[tree.plans[address].issue]; issue != nil {
			issue.plans = append(issue.plans, address)
		}
	}
	return tree, nil
}

// compareIssues orders two issues of the tree as the tree orders the children of one node
// (CompareSiblings), which is the tree's order of the issues under an issue: no issue is a
// conversation node, so one whose node holds a rank stands before one whose node holds none, two
// that hold one by rank, two others by when each node was made, and each tie by key.
func (tree *planTree) compareIssues(a, b string) int {
	left, right := tree.issues[a], tree.issues[b]
	return CompareSiblings(
		TreeChild{Node: Target{Kind: graphstore.KindIssue, ID: a}, Rank: left.rank, Ranked: left.ranked, MadeAt: left.madeAt},
		TreeChild{Node: Target{Kind: graphstore.KindIssue, ID: b}, Rank: right.rank, Ranked: right.ranked, MadeAt: right.madeAt})
}

// comparePlans orders two plan nodes of the tree in their document's order (R11): by text_rank,
// one that holds none first, and on a tie by address.
func (tree *planTree) comparePlans(a, b string) int {
	return cmp.Or(strings.Compare(tree.plans[a].textRank, tree.plans[b].textRank), strings.Compare(a, b))
}

// planList is the list the value text of the plan node address's attribute name holds, a JSON
// array of strings as the node's opening line writes a `string[]` attribute (planBlockValues), and
// an empty list, never nil, where the node holds no value of it (text is nil), so every reader
// answers a list.
func planList(address, name string, text *string) ([]string, error) {
	if text == nil {
		return []string{}, nil
	}
	list := []string{}
	if err := json.Unmarshal([]byte(*text), &list); err != nil {
		return nil, fmt.Errorf("read the %s of plan node %s, %q: %w", name, address, *text, err)
	}
	return list, nil
}

// readNeeds reads every current needs tuple out of the tree's issues and plan nodes, in one
// statement through graph_tuples_current, each far end one who may see.
func (tree *planTree) readNeeds(ctx context.Context, q Queryer, who model.Actor) error {
	kinds := make([]string, 0, len(tree.issues)+len(tree.plans))
	ids := make([]string, 0, len(tree.issues)+len(tree.plans))
	for key := range tree.issues {
		kinds, ids = append(kinds, graphstore.KindIssue), append(ids, key)
	}
	for address := range tree.plans {
		kinds, ids = append(kinds, BlockNodeKind), append(ids, address)
	}
	rows, err := q.Query(ctx, `
		select w.kind, w.id, held.to_kind, held.to_id
		from unnest($1::text[], $2::text[]) as w (kind, id)
		cross join lateral (
			`+graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = w.kind and t.from_id = w.id and t.kind = '`+graphstore.TupleNeeds+`'
			  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
		) held
		where held.asserted and `+VisibleNode("held.to_kind", "held.to_id", "$3"),
		kinds, ids, VisibilityArg(who))
	if err != nil {
		return fmt.Errorf("read the needs of the tree of %s: %w", tree.root, err)
	}
	var fromKind, fromID string
	var far Target
	if _, err := pgx.ForEachRow(rows, []any{&fromKind, &fromID, &far.Kind, &far.ID}, func() error {
		if fromKind == graphstore.KindIssue {
			tree.issues[fromID].needs = append(tree.issues[fromID].needs, far)
		} else {
			tree.plans[fromID].needs = append(tree.plans[fromID].needs, far)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the needs of the tree of %s: %w", tree.root, err)
	}
	return nil
}

// overtakenIssue reports whether issue key, or an issue above it in the tree, is overtaken.
func (tree *planTree) overtakenIssue(key string) bool {
	for held := tree.issues[key]; held != nil; held = tree.issues[held.parent] {
		if held.overtaken {
			return true
		}
	}
	return false
}

// halfOf names half as a node of the order.
func halfOf(half IssueHalf) OrderNode {
	if half.Done {
		return OrderNode{Kind: graphstore.KindIssue, ID: half.Key, Half: HalfDone}
	}
	return OrderNode{Kind: graphstore.KindIssue, ID: half.Key, Half: HalfStart}
}

// needed is the node a needs tuple's far end names: an issue's done half, as a start waits on
// the done of an issue it needs (IssueHalf.Waits), and any other node itself.
func needed(far Target) OrderNode {
	if far.Kind == graphstore.KindIssue {
		return halfOf(IssueHalf{Key: far.ID, Done: true})
	}
	return OrderNode{Kind: far.Kind, ID: far.ID}
}

// waits is every node the tree's node named node waits on, overtaken ones among them.
func (tree *planTree) waits(node OrderNode) []OrderNode {
	if node.Kind != graphstore.KindIssue {
		plan := tree.plans[node.ID]
		held := make([]OrderNode, 0, len(plan.needs)+1)
		held = append(held, halfOf(IssueHalf{Key: plan.issue}))
		for _, far := range plan.needs {
			held = append(held, needed(far))
		}
		return held
	}
	issue := tree.issues[node.ID]
	half := IssueHalf{Key: node.ID, Done: node.Half == HalfDone}
	waits := IssueWaits{Parent: issue.parent, Children: issue.children}
	var other []OrderNode
	for _, far := range issue.needs {
		if far.Kind == graphstore.KindIssue {
			waits.Needs = append(waits.Needs, far.ID)
		} else if !half.Done {
			other = append(other, needed(far))
		}
	}
	var held []OrderNode
	for _, own := range half.Halves()[1:] {
		held = append(held, halfOf(own))
	}
	for _, waited := range half.Waits(waits) {
		held = append(held, halfOf(waited))
	}
	if half.Done {
		for _, address := range issue.plans {
			held = append(held, OrderNode{Kind: BlockNodeKind, ID: address})
		}
	}
	return append(held, other...)
}

// baseOrder is the order the walk takes the tree's nodes in wherever their waits leave it a
// choice (R11, waitGraph.order): from the root down, an issue's start, its own plan nodes in their
// document's order, each issue under it in the tree's order with everything under it the same
// way, and last its done. The walk down the issues runs with a stack of its own.
func (tree *planTree) baseOrder() []OrderNode {
	base := make([]OrderNode, 0, 2*len(tree.issues)+len(tree.plans))
	type step struct {
		key  string
		done bool
	}
	walk := []step{{key: tree.root}}
	for len(walk) > 0 {
		at := walk[len(walk)-1]
		walk = walk[:len(walk)-1]
		base = append(base, halfOf(IssueHalf{Key: at.key, Done: at.done}))
		if at.done {
			continue
		}
		issue := tree.issues[at.key]
		for _, address := range issue.plans {
			base = append(base, OrderNode{Kind: BlockNodeKind, ID: address})
		}
		walk = append(walk, step{key: at.key, done: true})
		for _, child := range slices.Backward(issue.children) {
			walk = append(walk, step{key: child})
		}
	}
	return base
}

// order orders the tree (PlanOrder).
func (tree *planTree) order() TreeOrder {
	var placed, overtaken []OrderNode
	issue := map[OrderNode]string{}
	// Whether each issue, or one above it, is overtaken, read once per issue for its halves and
	// its plan nodes alike: an issue the tree does not hold is none.
	overtakenIssues := make(map[string]bool, len(tree.issues))
	for key := range tree.issues {
		overtakenIssue := tree.overtakenIssue(key)
		overtakenIssues[key] = overtakenIssue
		for _, half := range (IssueHalf{Key: key, Done: true}).Halves() {
			node := halfOf(half)
			issue[node] = key
			if overtakenIssue {
				overtaken = append(overtaken, node)
			} else {
				placed = append(placed, node)
			}
		}
	}
	for address, plan := range tree.plans {
		node := OrderNode{Kind: BlockNodeKind, ID: address}
		issue[node] = plan.issue
		if plan.overtaken || overtakenIssues[plan.issue] {
			overtaken = append(overtaken, node)
		} else {
			placed = append(placed, node)
		}
	}
	graph := newWaitGraph(placed, tree.baseOrder())
	gone := make(map[OrderNode]bool, len(overtaken))
	for _, node := range overtaken {
		gone[node] = true
	}
	fixed := map[OrderNode]bool{}
	answer := func(node OrderNode) OrderedNode {
		ordered := OrderedNode{OrderNode: node, Issue: issue[node], Skills: []string{}, Docs: []string{}}
		if node.Kind == BlockNodeKind {
			plan := tree.plans[node.ID]
			ordered.Size, ordered.Skills, ordered.Docs = plan.size, plan.skills, plan.docs
		}
		for _, waited := range tree.waits(node) {
			if gone[waited] {
				continue
			}
			if !graph.wait(node, waited) && !gone[node] {
				fixed[waited] = true
			}
			ordered.Needs = append(ordered.Needs, waited)
		}
		slices.SortFunc(ordered.Needs, compareOrderNodes)
		ordered.Needs = slices.Compact(ordered.Needs)
		return ordered
	}
	nodes := make([]OrderedNode, len(graph.nodes))
	for index, node := range graph.nodes {
		nodes[index] = answer(node)
	}
	var result TreeOrder
	slices.SortFunc(overtaken, compareOrderNodes)
	for _, node := range overtaken {
		result.Overtaken = append(result.Overtaken, answer(node))
	}
	order, cycles := graph.order()
	for _, index := range order {
		result.Order = append(result.Order, nodes[index])
	}
	for _, cycle := range cycles {
		held := make([]OrderedNode, len(cycle))
		for position, index := range cycle {
			held[position] = nodes[index]
		}
		result.Cycles = append(result.Cycles, held)
	}
	result.Fixed = slices.SortedFunc(maps.Keys(fixed), compareOrderNodes)
	return result
}

// compareOrderNodes orders the nodes of the ordered read by id, then kind, then an issue's start
// before its done: the order every list of the answer is sorted in, a cycle's nodes among them.
func compareOrderNodes(a, b OrderNode) int {
	return cmp.Or(strings.Compare(a.ID, b.ID), strings.Compare(a.Kind, b.Kind), halfRank(a.Half)-halfRank(b.Half))
}

// halfRank is where a half sorts among the nodes of one id: a plan node's none, then start, then
// done.
func halfRank(half string) int {
	switch half {
	case HalfStart:
		return 1
	case HalfDone:
		return 2
	}
	return 0
}

// waitGraph is the nodes the ordered read places, sorted (compareOrderNodes), what each of them
// waits on among them, by position in nodes, and each one's rank in the base order the walk takes
// them in (order), from 0.
type waitGraph struct {
	nodes []OrderNode
	index map[OrderNode]int
	waits [][]int
	rank  []int
}

// newWaitGraph is the graph of nodes, which it sorts, waiting on nothing yet, each ranked at its
// place among them in base, the base order (planTree.baseOrder); a node base does not hold ranks
// after every node it holds, in the sort.
func newWaitGraph(nodes, base []OrderNode) *waitGraph {
	sorted := slices.SortedFunc(slices.Values(nodes), compareOrderNodes)
	sorted = slices.Compact(sorted)
	graph := &waitGraph{nodes: sorted, index: make(map[OrderNode]int, len(sorted)), waits: make([][]int, len(sorted)), rank: make([]int, len(sorted))}
	for position, node := range sorted {
		graph.index[node], graph.rank[position] = position, -1
	}
	ranked := 0
	for _, node := range base {
		if position, held := graph.index[node]; held && graph.rank[position] < 0 {
			graph.rank[position], ranked = ranked, ranked+1
		}
	}
	for position, rank := range graph.rank {
		if rank < 0 {
			graph.rank[position], ranked = ranked, ranked+1
		}
	}
	return graph
}

// wait records that from waits on to, and reports whether to is a node of the graph: a wait on
// a node it does not hold orders nothing.
func (g *waitGraph) wait(from, to OrderNode) bool {
	target, held := g.index[to]
	if !held {
		return false
	}
	if source, holds := g.index[from]; holds {
		g.waits[source] = append(g.waits[source], target)
	}
	return true
}

// order answers the graph's nodes in no cycle, by position, each after every node it waits on
// that the order places, and its cycles, each by position and the cycles by their first node.
// The components are Tarjan's (stronglyConnected), and they are placed by R11's needs-first
// depth-first walk, run with a stack of its own so a long chain costs no depth: it starts from
// each component that no node outside it needs, by its first node in the base order (rank), and
// before it places a component it places each component it waits on that is not placed yet, the
// same way, first those that no node needs and then the rest, each in the base order. The waits
// of a done half need nothing: a done closes its issue, and the steps it waits on keep the order
// the walk gives them without it. A cycle places none of its nodes, and a node that waits on one
// keeps its place.
func (g *waitGraph) order() (order []int, cycles [][]int) {
	for node := range g.waits {
		slices.Sort(g.waits[node])
		g.waits[node] = slices.Compact(g.waits[node])
	}
	component, count := stronglyConnected(g.waits)
	members := make([][]int, count)
	first := make([]int, count)
	for held := range first {
		first[held] = len(g.nodes)
	}
	for node, held := range component {
		members[held] = append(members[held], node)
		first[held] = min(first[held], g.rank[node])
	}
	// What each component waits on, and whether a node outside it that is no done half needs it.
	before := make([][]int, count)
	needed := make([]bool, count)
	for node, targets := range g.waits {
		for _, target := range targets {
			if from, to := component[node], component[target]; from != to {
				before[from] = append(before[from], to)
				needed[to] = needed[to] || g.nodes[node].Half != HalfDone
			}
		}
	}
	walkOrder := func(a, b int) int {
		if needed[a] != needed[b] {
			if needed[a] {
				return 1
			}
			return -1
		}
		return first[a] - first[b]
	}
	for held := range before {
		slices.SortFunc(before[held], walkOrder)
		before[held] = slices.Compact(before[held])
	}
	var starts []int
	for held := range count {
		if !needed[held] {
			starts = append(starts, held)
		}
	}
	slices.SortFunc(starts, walkOrder)
	reached := make([]bool, count)
	type frame struct{ held, next int }
	var walk []frame
	for _, start := range starts {
		if reached[start] {
			continue
		}
		reached[start] = true
		walk = append(walk, frame{held: start})
		for len(walk) > 0 {
			top := &walk[len(walk)-1]
			if top.next < len(before[top.held]) {
				next := before[top.held][top.next]
				top.next++
				if !reached[next] {
					reached[next] = true
					walk = append(walk, frame{held: next})
				}
				continue
			}
			nodes := members[top.held]
			walk = walk[:len(walk)-1]
			if len(nodes) > 1 || slices.Contains(g.waits[nodes[0]], nodes[0]) {
				cycles = append(cycles, nodes)
			} else {
				order = append(order, nodes[0])
			}
		}
	}
	slices.SortFunc(cycles, func(a, b []int) int { return a[0] - b[0] })
	return order, cycles
}

// stronglyConnected numbers the strongly connected component of each node of waits, by Tarjan's
// algorithm run with a stack of its own rather than the call stack, so a chain of any length
// costs no depth: component[node] is the node's, numbered from 0 in the order they close, and
// count is how many there are.
func stronglyConnected(waits [][]int) (component []int, count int) {
	nodes := len(waits)
	visit := make([]int, nodes) // the order each node was first reached in, from 1; 0 for none yet
	low := make([]int, nodes)
	open := make([]bool, nodes)
	component = make([]int, nodes)
	var stack []int
	type frame struct{ node, next int }
	var calls []frame
	visited := 0
	reach := func(node int) {
		visited++
		visit[node], low[node] = visited, visited
		stack, open[node] = append(stack, node), true
		calls = append(calls, frame{node: node})
	}
	for root := range nodes {
		if visit[root] != 0 {
			continue
		}
		reach(root)
		for len(calls) > 0 {
			top := &calls[len(calls)-1]
			node := top.node
			if top.next < len(waits[node]) {
				target := waits[node][top.next]
				top.next++
				switch {
				case visit[target] == 0:
					reach(target)
				case open[target]:
					low[node] = min(low[node], visit[target])
				}
				continue
			}
			calls = calls[:len(calls)-1]
			if len(calls) > 0 {
				caller := calls[len(calls)-1].node
				low[caller] = min(low[caller], low[node])
			}
			if low[node] != visit[node] {
				continue
			}
			for {
				member := stack[len(stack)-1]
				stack, open[member] = stack[:len(stack)-1], false
				component[member] = count
				if member == node {
					break
				}
			}
			count++
		}
	}
	return component, count
}
