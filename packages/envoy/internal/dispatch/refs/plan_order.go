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
// is the first one its part_of chain reaches, whatever its address spells, a block of the
// issue's asks, comments and messages reaching their issue. A plan node waits on
// its issue's start, and its issue's done waits on it; a needs tuple out of a plan node waits on
// the plan node it names, or on the done half of the issue it names. A node the tree does not
// hold is a fixed node. An issue the reader may not see, and every node under it, is no node of
// the tree, and a node that waits on one waits on nothing there; an issueKey the reader may not
// see answers nothing.
//
// It reads the tree in two statements beside IssueDescendants', however large: the walk down
// the current part_of tuples into each issue and into each current block of its asks, comments
// and messages (graph_tuples_inbound), through every node who may see and never into an issue,
// which answers each issue's parent, its node's current rank and when its node was made, and
// each plan node with its issue, its current text_rank, size, skills
// and docs values (graph_tuples_values), which the block index states from the node's place in
// its document and its opening line (planBlockValues), and whether it or a node above it is
// overtaken; and one read of every current needs tuple out of those nodes (graph_tuples_current).
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
// overtaken, every node under it on its part_of chain, and an overtaken issue with every issue
// and node under it get no position.
func PlanOrder(ctx context.Context, q Queryer, who model.Actor, issueKey string) (TreeOrder, error) {
	if issueKey == "" {
		return TreeOrder{}, nil
	}
	descendants, err := IssueDescendants(ctx, q, who, issueKey)
	if err != nil {
		return TreeOrder{}, err
	}
	tree, err := readPlanTree(ctx, q, who, issueKey, descendants)
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

// planConversationBlocks is the second start of readPlanTree's walk, a lateral read over the issue
// i: each current block (itemBlockCurrent) of each of its asks, comments and messages, by its kind
// as the walk of the tree takes it (ItemKind.BlockKind) and its node id. A criterion a person
// writes in that exchange, and every node under it, stands under such a block, and the
// conversation holds no part_of tuple into the issue - the walk of the tree reads it apart and
// walks down from its blocks (docs' readConversation, TreeSubtrees) - so the walk down the issue's
// own part_of tuples reaches none of them.
func planConversationBlocks() string {
	arms := make([]string, len(itemKinds))
	for index, item := range itemKinds {
		tables, _ := item.tables()
		arms[index] = `select '` + item.BlockKind() + `'::text as kind, b.node_id from ` + tables.items + ` item
				join ` + tables.blocks + ` b on b.` + tables.item + ` = item.id
				where item.issue_key = i.key and ` + itemBlockCurrent
	}
	return strings.Join(arms, `
				union all
				`)
}

// readPlanTree reads the tree of root, whose descendants are descendants, in one statement: the
// walk down each issue's current part_of tuples, from the issue's node and from each current block
// of its asks, comments and messages (planConversationBlocks), which answers a row per issue and a
// row per plan node (PlanOrder).
func readPlanTree(ctx context.Context, q Queryer, who model.Actor, root string, descendants []string) (*planTree, error) {
	rows, err := q.Query(ctx, `
		with recursive below (issue, parent, kind, id, depth, state, overtaken, made) as (
			select i.key, i.parent_key, '`+graphstore.KindIssue+`'::text, i.key, 0, n.state,
			       coalesce(n.state = '`+graphstore.StateOvertaken+`', false), n.made_at
			from issues i
			left join graph_nodes n on n.kind = '`+graphstore.KindIssue+`' and n.id = i.key
			where i.key = any($1::text[]) and `+VisibleKind(graphstore.KindIssue, "i.key", "$3")+`
			union all
			select i.key, null::text, conversation.kind, conversation.node_id, 1, null::text,
			       coalesce(n.state = '`+graphstore.StateOvertaken+`', false), null::timestamptz
			from issues i
			left join graph_nodes n on n.kind = '`+graphstore.KindIssue+`' and n.id = i.key
			cross join lateral (`+planConversationBlocks()+`) conversation
			where i.key = any($1::text[]) and `+VisibleKind(graphstore.KindIssue, "i.key", "$3")+`
			union all
			select below.issue, null::text, held.from_kind, held.from_id, below.depth + 1, n.state,
			       below.overtaken or coalesce(n.state = '`+graphstore.StateOvertaken+`', false), null::timestamptz
			from below
			cross join lateral (
				`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id`, `t.from_kind, t.from_id, t.asserted`,
		`t.to_kind = below.kind and t.to_id = below.id and t.kind = '`+graphstore.TuplePartOf+`'
				  and t.to_kind <> '`+graphstore.KindValue+`'`)+`
			) held
			left join graph_nodes n on n.kind = held.from_kind and n.id = held.from_id
			where held.asserted and held.from_kind <> '`+graphstore.KindIssue+`' and below.depth < $2
			  and `+VisibleNodeOfRow("held.from_kind", "held.from_id", "n", "$3")+`
		)
		select found.issue, found.parent, found.id, found.root, found.overtaken, found.made,
		       case when ranked.asserted then ranked.value end,
		       case when sized.asserted then sized.value end, case when skilled.asserted then skilled.value end,
		       case when documented.asserted then documented.value end
		from (
			select below.issue, below.parent, below.kind, below.id, below.depth, below.depth = 0 as root, below.overtaken, below.made
			from below
			where below.depth = 0
			   or (below.kind = '`+BlockNodeKind+`' and coalesce(below.state, '`+graphstore.StateLive+`') <> '`+graphstore.StateGone+`'
			       and exists (select 1 from document_blocks d where d.node_id = below.id and d.type = '`+planTaskType+`'))
		) found
		left join lateral (
			select t.to_id as value, t.asserted from graph_tuples t
			where t.from_kind = found.kind and t.from_id = found.id
			  and t.kind = case when found.root then '`+AttributeRank+`' else '`+AttributeTextRank+`' end
			  and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) ranked on true
		left join lateral (
			select t.to_id as value, t.asserted from graph_tuples t
			where not found.root and t.from_kind = '`+BlockNodeKind+`' and t.from_id = found.id
			  and t.kind = '`+AttributePlanSize+`' and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) sized on true
		left join lateral (
			select t.to_id as value, t.asserted from graph_tuples t
			where not found.root and t.from_kind = '`+BlockNodeKind+`' and t.from_id = found.id
			  and t.kind = '`+AttributePlanSkills+`' and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) skilled on true
		left join lateral (
			select t.to_id as value, t.asserted from graph_tuples t
			where not found.root and t.from_kind = '`+BlockNodeKind+`' and t.from_id = found.id
			  and t.kind = '`+AttributePlanDocs+`' and t.to_kind = '`+graphstore.KindValue+`'
			order by t.seq desc limit 1
		) documented on true
		order by found.id, found.depth, found.issue
	`, append([]string{root}, descendants...), planOrderDepth, VisibilityArg(who))
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
