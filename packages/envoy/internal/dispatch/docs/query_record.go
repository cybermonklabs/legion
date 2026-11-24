package docs

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// An approval covers the digest of one query's record. A query is a filter of the
// issue's one tree: it walks the tree in the tree's own order, keeps the nodes it takes, and writes
// each in full into a record whose sha256 is the digest. One walk and one builder answer the head
// and every version: the head is read at the newest seq the tuple log holds, a version at the
// read_seq its record was read at (refs.TreeCut), and nothing else tells the two apart.

// The type names the query definitions read: the memo is a callout, and a result stands under a
// check.
const (
	memoType  = "callout"
	checkType = graphstore.BlockTypeCheck
)

// queryDefinitions is the one definition of each query whose record is built: which of the walk's
// nodes it takes, at their index in the walk. Each takes nodes of the types graphstore.QueryTakes
// lists for it (the takes value its query node carries), by the rule it adds. The spec takes the
// memo - the issue's first child in the tree's order, when that child is a callout; no other
// callout is the memo, and none enters the query - and the criteria and the requirements; on an
// issue that is a view (IssueTree.ViewOf) it takes the memo alone, so a requirement a client
// brought into a view through its room is no node of the view's spec and no part of the record an
// approval of it covers (R23). The verification takes the checks and the results under them. The
// plan takes the plan nodes. The decisions query takes asks, decisions and readings by a rule of
// its own (decisionsTakes), which the tree read's decisions view answers its nodes by (Takes); it
// holds no entry here until its record is built, so a record of it is refused, as a record of a
// query with no entry is.
var queryDefinitions = map[string]func(tree IssueTree, index int) bool{
	graphstore.QuerySpec: func(tree IssueTree, index int) bool {
		if tree.ViewOf != "" {
			return index == tree.Memo
		}
		typ := tree.Nodes[index].Type
		return slices.Contains(queryTakes[graphstore.QuerySpec], typ) && (typ != memoType || index == tree.Memo)
	},
	graphstore.QueryVerification: func(tree IssueTree, index int) bool {
		node := tree.Nodes[index]
		if !slices.Contains(queryTakes[graphstore.QueryVerification], node.Type) {
			return false
		}
		if node.Type != graphstore.SupertagResult {
			return true
		}
		parent, held := tree.index[node.Parent]
		return held && tree.Nodes[parent].Type == checkType
	},
	graphstore.QueryPlan: func(tree IssueTree, index int) bool {
		return slices.Contains(queryTakes[graphstore.QueryPlan], tree.Nodes[index].Type)
	},
}

// queryTakes is graphstore.QueryTakes of each query a definition holds, and of the decisions
// query, whose rule is decisionsTakes, read once.
var queryTakes = map[string][]string{
	graphstore.QuerySpec:         graphstore.QueryTakes(graphstore.QuerySpec),
	graphstore.QueryVerification: graphstore.QueryTakes(graphstore.QueryVerification),
	graphstore.QueryPlan:         graphstore.QueryTakes(graphstore.QueryPlan),
	graphstore.QueryDecisions:    graphstore.QueryTakes(graphstore.QueryDecisions),
}

// decisionsTakes is the decisions query's rule: of the types graphstore.QueryTakes lists for it,
// each ask by the ask's own node, never the block that shows it in a document; each decision; and
// each reading of a decision, whose parent in the walk is a decision or a person's answered ask,
// the decision that ask is. A reading in an ask a person answered stands under the ask's own node
// (refs.DecisionFacts places its part_of there), and the decisions read lists it under that ask's
// entry; a reading in an ask nobody has answered is no reading yet and stands under the ask's
// block, which this rule never takes it under.
func decisionsTakes(tree IssueTree, index int) bool {
	node := tree.Nodes[index]
	if !slices.Contains(queryTakes[graphstore.QueryDecisions], node.Type) {
		return false
	}
	switch node.Type {
	case graphstore.SupertagAsk:
		return node.Kind == graphstore.KindAsk
	case refs.ReadingSupertag:
		at, held := tree.index[node.Parent]
		if !held {
			return false
		}
		parent := tree.Nodes[at]
		return parent.Type == refs.DecisionSupertag || parent.Kind == graphstore.KindAsk
	}
	return true
}

// TreeNode is one node of an issue's tree as a walk takes it: its kind and its id, as the store
// keys it; the node it stands under in the walk; its type (refs.TreeNodeType: its supertag, or for
// a block the typed block its text opens), which a query takes it by; its rank, and whether it
// holds one; when it was made, which orders it among its unranked siblings; whether it stands in
// the walk's second part, under a node of an issue above or in such a node's subtree; and its
// supertag and whether the walk read its row (Row). The walk reads the row of each node it takes
// off a children read (refs.TreeChild), so Supertag and MadeAt are then what refs.TreeNodeRows
// answers of it - the empty supertag and the zero time where the store holds no row; it reads no
// row of the issue's node, nor of its conversation's items and their blocks.
type TreeNode struct {
	Kind, ID string
	Parent   refs.Target
	Type     string
	Rank     string
	Ranked   bool
	MadeAt   time.Time
	Above    bool
	Supertag string
	Row      bool
}

// Address is the node's store address, `<kind>:<id>`, which no other node holds: the record names
// a node and each far end by it, and the walk compares two by it, byte for byte, where nothing else
// tells them apart.
func (node TreeNode) Address() string { return node.Kind + ":" + node.ID }

func (node TreeNode) target() refs.Target { return refs.Target{Kind: node.Kind, ID: node.ID} }

// IssueTree is the walk of one issue's tree at one cut: the issue's key, the cut, every node in
// the walk's order - the issue's own node first - the index of the memo among them, -1 where the
// issue's first child is no callout, the issue it is a view of (ViewOf), "" where it is none, the
// issues above it, parent first (refs.IssueAncestors), which the walk's second part reads, and
// every row the walk's reads of the subtrees answered (refs.TreeSubtrees), keyed by the parent, in
// the order those reads answer them: the children of each node the walk took, those it does not
// enter - an issue's node, a node of another issue - among them, as refs.TreeChildren answers the
// children of the walk's nodes at the walk's cut; and each node of the conversation as its loader
// answered it with its excerpt (refs.ConversationNodes), keyed as the walk keys the node. A reader
// of the walk in the same transaction takes these from here rather than reading them again. A
// skeleton walk (WalkIssueSkeleton) also names, for each comment it hangs under the issue because
// it does not take the comment's parent, that parent (Named); and Hidden is each document it does
// not enter, and each block of one, that a node it does not leave out stands under, whose rows are
// in Children, in address order.
type IssueTree struct {
	Issue        string
	Cut          refs.TreeCut
	Nodes        []TreeNode
	Memo         int
	ViewOf       string
	Ancestors    []string
	Children     map[refs.Target][]refs.TreeChild
	Conversation map[refs.Target]refs.ExcerptedNode
	Named        map[refs.Target]refs.Target
	Hidden       []refs.Target
	index        map[refs.Target]int
}

// WalkIssueTree walks the tree of the issue issueKey at cut, the head's where cut is zero
// (refs.HeadCut), in two parts.
//
// The first part starts at the issue's node and takes only the issue's own nodes, each before its
// children. A node's children are the nodes whose part_of names it at the cut and, for the issue's
// own node, each ask, comment and message of the issue that the conversation test passes at the
// cut (refs.ConversationNodes) whose loader names no parent that passes it too. An item whose loader
// names such a parent hangs under it: a reply under its head, a comment in an ask's thread under
// the ask, a reply to a block under that block. A comment written on a document whose anchor lies
// inside a typed block hangs under that block's node - the anchor's own block where it is typed,
// else its nearest typed ancestor - read through the comment's anchored_in at the cut and, where
// that address is no node of the walk, through the moved_to tuples that carry it to one (the
// node in a converted issue's room); every other item hangs under the issue. Under a conversation
// item, its own blocks come first, in their position order. Then, under every node, the ranked
// children by increasing rank and the unranked ones - an ask, a comment, a message, a lens, a risk,
// a result - by when each was made, an item whose row has no node yet by its row's created_at,
// each tie by the lower address. A rank and an address compare byte for byte. The walk does not
// enter an issue under the issue, nor a node of another issue, so a parent's record holds no node
// of a child.
//
// The second part takes each node of the issue whose parent is a node of an issue above
// (refs.IssueAncestors, refs.TreePlacedAbove), by the parent's address, then rank, then its own
// address, each with its subtree walked as the first part walks it. The walk of the issue above
// passes over such a node.
//
// The walk binds no reader's visibility: it is what the record a person approves is built from,
// and what every gate compares a review's digest with, so it is the store as the server holds it,
// one answer for every reader. Were it a reader's, two readers of one version would read two
// digests, and a node hidden from the reader who asked would drop out of the record its approver
// read. A render of the tree leaves out what its reader may not see; the record does not.
func WalkIssueTree(ctx context.Context, q Queryer, issueKey string, cut refs.TreeCut) (IssueTree, error) {
	if cut == (refs.TreeCut{}) {
		head, err := refs.HeadCut(ctx, q)
		if err != nil {
			return IssueTree{}, err
		}
		cut = head
	}
	return walkIssueTree(ctx, q, issueKey, cut, nil, nil)
}

// WalkIssueHead is WalkIssueTree at the head cut cut, its nodes' children, rows and anchors read
// from the issue's rows of the head projection, heads (refs.HeadRows), rather than from the tuples:
// the same two parts, in the same order. A node of the conversation no row holds - one a build that
// keeps no head rows wrote - is read from the store as the walk reads it. The head read of the tree
// (api's tree read with no version) walks so; the record of every query, and every read at a
// version, walks the tuples.
func WalkIssueHead(ctx context.Context, q Queryer, issueKey string, cut refs.TreeCut, heads *refs.HeadTree) (IssueTree, error) {
	return walkIssueTree(ctx, q, issueKey, cut, heads, nil)
}

// Skeleton is what the walk of the skeleton of an issue's tree takes, which the tree read with no
// view answers (R8): every node the walk takes but the blocks of the issue's documents other than
// Documents, each the ref_key of a document of the issue whose blocks the skeleton holds, and but
// the comments of the conversation other than those of the comment page, Comments. The walk takes
// the node of every other document of the issue and enters nothing under it, so it reads no block
// of it, and it reads the comments of the page alone (refs.ConversationPageNodes).
type Skeleton struct {
	Documents map[string]bool
	Comments  refs.CommentPage
}

// WalkIssueSkeleton is the walk of the skeleton of the issue issueKey's tree at cut (Skeleton),
// read from the head rows heads where they are given (WalkIssueHead) and from the tuples where
// heads is nil. A comment it hangs under the issue because it does not take the comment's parent
// - a comment of another page of the conversation, or the typed block of another document that
// the comment's anchor lies in - names that parent in Named. The tree read with no view walks so.
func WalkIssueSkeleton(ctx context.Context, q Queryer, issueKey string, cut refs.TreeCut, heads *refs.HeadTree, skeleton Skeleton) (IssueTree, error) {
	return walkIssueTree(ctx, q, issueKey, cut, heads, &skeleton)
}

// walkIssueTree is the walk of the issue issueKey's tree at cut, read from heads where it is set
// and from the tuples otherwise, the walk of its skeleton where skeleton is set.
func walkIssueTree(ctx context.Context, q Queryer, issueKey string, cut refs.TreeCut, heads *refs.HeadTree, skeleton *Skeleton) (IssueTree, error) {
	walk := treeWalk{
		issue:    refs.Target{Kind: graphstore.KindIssue, ID: issueKey},
		owner:    issueKey,
		cut:      cut,
		heads:    heads,
		skeleton: skeleton,
		nodes:    map[refs.Target]TreeNode{},
		children: map[refs.Target][]refs.Target{},
		under:    map[refs.Target][]refs.TreeChild{},
		blocks:   map[refs.Target][]refs.Target{},
	}
	if err := walk.readConversation(ctx, q); err != nil {
		return IssueTree{}, err
	}
	frontier := append([]refs.Target{walk.issue}, walk.conversation...)
	if err := walk.readSubtrees(ctx, q, frontier, false); err != nil {
		return IssueTree{}, err
	}
	lifted := walk.liftHidden()
	// The issues above it, parent first, and whether it is a view are read in one statement
	// (viewChainStatement, as ViewOf reads it): the chain from the issue up, whose rows past the
	// issue's own are refs.IssueAncestors' issues, in its order and to its depth.
	chain, err := readViewChain(ctx, q, issueKey, 1)
	if err != nil {
		return IssueTree{}, fmt.Errorf("read the issues above %s: %w", issueKey, err)
	}
	ancestors := chainAncestors(chain)
	above, err := refs.TreePlacedAbove(ctx, q, issueKey, ancestors, cut)
	if err != nil {
		return IssueTree{}, err
	}
	var roots []refs.Target
	for _, child := range above {
		if _, seen := walk.nodes[child.Node]; seen {
			continue
		}
		walk.nodes[child.Node] = treeNodeOf(child, true)
		roots = append(roots, child.Node)
	}
	if err := walk.readSubtrees(ctx, q, roots, true); err != nil {
		return IssueTree{}, err
	}
	if err := walk.placeItems(ctx, q); err != nil {
		return IssueTree{}, err
	}

	tree := IssueTree{Issue: issueKey, Cut: cut, Memo: -1, Ancestors: ancestors, Children: walk.under, Conversation: walk.excerpts, Named: walk.named,
		Hidden: walk.hidden, index: map[refs.Target]int{}}
	// Whether the issue is a view is no fact of the tree: it is read from the issue's record as it
	// stands (ViewOf), as every reader of it reads it, whatever the cut.
	tree.ViewOf = viewOfIssue(chain)
	walk.nodes[walk.issue] = TreeNode{Kind: walk.issue.Kind, ID: walk.issue.ID, Type: graphstore.SupertagIssue}
	walk.take(&tree, walk.issue)
	if first := walk.ordered(walk.issue); len(first) > 0 && walk.nodes[first[0]].Type == memoType {
		tree.Memo = 1
	}
	for _, root := range slices.Concat(lifted, roots) {
		walk.take(&tree, root)
	}
	return tree, nil
}

// treeWalk is what a walk has read of the issue's tree: its root, the issue's node, and the owner
// every address of the issue's own nodes begins with, the issue's key (refs.TreeOwns); every node it
// took, by its address; the part_of children of each node, ranked first, in the order the children
// read answers them; every row its reads of the subtrees answered, by the parent (IssueTree's
// Children); the conversation's items and their blocks, in the order the conversation read
// answers them, the items alone, the blocks of each item, the parent each item's loader names and
// each node as its loader answered it (IssueTree's Conversation); and the items that hang under
// each node. heads is the issue's rows of the head projection a head walk reads in place of the
// tuples (WalkIssueHead), nil for a walk of the tuples; skeleton is what a walk of the skeleton
// takes (WalkIssueSkeleton), nil for the walk of the whole tree; named each parent such a walk
// names for a comment it hangs under the issue (IssueTree's Named); and hidden each block it does
// not enter whose children it read (IssueTree's Hidden).
type treeWalk struct {
	issue        refs.Target
	owner        string
	cut          refs.TreeCut
	heads        *refs.HeadTree
	skeleton     *Skeleton
	nodes        map[refs.Target]TreeNode
	children     map[refs.Target][]refs.Target
	under        map[refs.Target][]refs.TreeChild
	conversation []refs.Target
	items        []refs.Target
	blocks       map[refs.Target][]refs.Target
	loaded       map[refs.Target]*model.NodeParent
	excerpts     map[refs.Target]refs.ExcerptedNode
	hanging      map[refs.Target][]refs.Target
	named        map[refs.Target]refs.Target
	hidden       []refs.Target
}

// enters is whether the walk enters node, reading what stands under it: every node but, on a walk
// of the skeleton, a document of the issue whose blocks the skeleton does not hold.
func (w *treeWalk) enters(node refs.Target) bool {
	return w.skeleton == nil || node.Kind != graphstore.KindArtifact || w.skeleton.Documents[node.ID] ||
		!strings.HasPrefix(node.ID, w.owner+"/")
}

// hiddenBlock is whether node is a block of a document of the issue a walk of the skeleton does not
// enter: its address begins with that document's ref_key and `#` (refs.BlockAddress).
func (w *treeWalk) hiddenBlock(node refs.Target) bool {
	document, _, cut := strings.Cut(node.ID, "#")
	return node.Kind == refs.BlockNodeKind && cut && !w.enters(refs.Target{Kind: graphstore.KindArtifact, ID: document})
}

// hiddenParent is whether node is a parent a walk of the skeleton takes nothing under by its
// levels: a document of the issue it does not enter, which it takes, and a block of one, which it
// does not (hiddenBlock).
func (w *treeWalk) hiddenParent(node refs.Target) bool {
	return w.hiddenBlock(node) || !w.enters(node)
}

// liftHidden takes, on a walk of the skeleton, each node of the issue that stands under a document
// the walk does not enter, or under a block of one, and is no block of such a document - a lens on
// the document or on one of its blocks, with its findings - and everything under it, as the walk
// takes any node, its parent the node it stands under, which the tree read lifts to the issue
// (R8). It keeps each such parent that holds a child other than such a block in hidden, in address
// order, with its children in under, so the tree read finds there a node of another issue placed
// on one. It reads nothing: the head rows of a skeleton hold every row of a kind other than a
// block, and the walk of the tuples read every level under the issue already. It answers the
// nodes it took that such a parent holds, in the order of their parents and then the children
// read's.
func (w *treeWalk) liftHidden() []refs.Target {
	if w.skeleton == nil {
		return nil
	}
	hidden := map[refs.Target][]refs.TreeChild{}
	if w.heads != nil {
		hidden = w.heads.Under(w.hiddenParent)
	} else {
		for parent, children := range w.under {
			if w.hiddenParent(parent) {
				hidden[parent] = children
			}
		}
	}
	var lifted []refs.Target
	for _, parent := range slices.SortedFunc(maps.Keys(hidden), refs.CompareAddresses) {
		holds := false
		for _, child := range hidden[parent] {
			if w.hiddenBlock(child.Node) {
				continue
			}
			holds = true
			if _, seen := w.nodes[child.Node]; seen || !refs.TreeOwns(w.owner, child.Node) {
				continue
			}
			w.nodes[child.Node] = treeNodeOf(child, false)
			lifted = append(lifted, child.Node)
		}
		if holds {
			w.hidden = append(w.hidden, parent)
			w.under[parent] = hidden[parent]
		}
	}
	under := w.under
	if w.heads != nil {
		under = w.heads.Subtrees(lifted, w.owner)
		for parent, children := range under {
			if _, held := w.under[parent]; !held {
				w.under[parent] = children
			}
		}
	}
	w.takeLevels(lifted, under, false)
	return lifted
}

// subtrees is the children of each node of frontier and of each node under them the walk enters
// (refs.TreeSubtrees), read from the head rows where the walk reads them.
func (w *treeWalk) subtrees(ctx context.Context, q Queryer, frontier []refs.Target) (map[refs.Target][]refs.TreeChild, error) {
	if w.heads != nil {
		return w.heads.Subtrees(frontier, w.owner), nil
	}
	return refs.TreeSubtrees(ctx, q, frontier, w.owner, w.cut)
}

// nodeRows is the row of each of nodes the store holds (refs.TreeNodeRows): a head walk's from the
// head row of each node one holds, and from the store for the rest.
func (w *treeWalk) nodeRows(ctx context.Context, q Queryer, nodes []refs.Target) (map[refs.Target]refs.TreeNodeRow, error) {
	if w.heads == nil {
		return refs.TreeNodeRows(ctx, q, nodes)
	}
	rows := map[refs.Target]refs.TreeNodeRow{}
	var unheld []refs.Target
	for _, node := range nodes {
		head, held := w.heads.Rows[node]
		switch {
		case !held:
			unheld = append(unheld, node)
		case head.Stored:
			rows[node] = head.Row()
		}
	}
	read, err := refs.TreeNodeRows(ctx, q, unheld)
	if err != nil {
		return nil, err
	}
	maps.Copy(rows, read)
	return rows, nil
}

// facts is what each of nodes states at the walk's cut (refs.TreeFactsOf): a head walk's from the
// head row of each node one holds, and from the store for the rest.
func (w *treeWalk) facts(ctx context.Context, q Queryer, nodes []refs.Target) (map[refs.Target]refs.TreeFacts, error) {
	if w.heads == nil {
		return refs.TreeFactsOf(ctx, q, nodes, w.cut)
	}
	facts := map[refs.Target]refs.TreeFacts{}
	var unheld []refs.Target
	for _, node := range nodes {
		head, held := w.heads.Rows[node]
		switch {
		case !held:
			unheld = append(unheld, node)
		case head.Facts.Values != nil || head.Facts.Relations != nil:
			facts[node] = head.Facts
		}
	}
	read, err := refs.TreeFactsOf(ctx, q, unheld, w.cut)
	if err != nil {
		return nil, err
	}
	maps.Copy(facts, read)
	return facts, nil
}

// readConversation reads the issue's conversation at the walk's cut (refs.ConversationNodes), the
// comments of the comment page alone on a walk of the skeleton (refs.ConversationPageNodes), read
// off the issue's head rows where the walk reads them and they hold the page
// (refs.ConversationPageNodesOfHeads): each item and each of its blocks as a node of the walk, the
// blocks of each item in their order, and the parent each item's loader names. Every item, and
// every block of one, is a node part_of may place a node under, so each starts a subtree of its
// own.
//
// It reads as the server sees the store: no ask, comment or message is a node the store takes
// reserved for its author (graphstore.AdmitAuthorOnly), so no reader's predicate leaves one out,
// and the empty actor is the reader.
func (w *treeWalk) readConversation(ctx context.Context, q Queryer) error {
	var conversation []refs.ExcerptedNode
	var err error
	held := false
	if w.skeleton != nil && w.heads != nil {
		conversation, held, err = refs.ConversationPageNodesOfHeads(ctx, q, model.Actor{}, w.issue.ID, w.cut.At, w.skeleton.Comments, w.heads)
	}
	switch {
	case err != nil || held:
	case w.skeleton != nil:
		conversation, err = refs.ConversationPageNodes(ctx, q, model.Actor{}, w.issue.ID, w.cut.At, w.skeleton.Comments)
	default:
		conversation, err = refs.ConversationNodes(ctx, q, model.Actor{}, w.issue.ID, w.cut.At)
	}
	if err != nil {
		return fmt.Errorf("read the conversation of %s: %w", w.issue.ID, err)
	}
	w.loaded = map[refs.Target]*model.NodeParent{}
	w.excerpts = make(map[refs.Target]refs.ExcerptedNode, len(conversation))
	for _, excerpted := range conversation {
		node := excerpted.Node
		if kind, itemID, _, ok := refs.ParseItemBlockNode(node.ID); ok {
			block := refs.Target{Kind: kind.BlockKind(), ID: node.ID}
			item := refs.Target{Kind: string(kind), ID: itemID}
			w.blocks[item] = append(w.blocks[item], block)
			w.nodes[block] = TreeNode{Kind: block.Kind, ID: block.ID, Parent: item, Type: block.Kind}
			w.conversation = append(w.conversation, block)
			w.excerpts[block] = excerpted
			continue
		}
		item := refs.Target{Kind: node.Kind, ID: node.ID}
		made := time.Time{}
		if node.MadeAt != nil {
			made = *node.MadeAt
		}
		w.nodes[item] = TreeNode{Kind: item.Kind, ID: item.ID, Type: item.Kind, MadeAt: made}
		w.loaded[item] = node.Parent
		w.excerpts[item] = excerpted
		w.conversation = append(w.conversation, item)
		w.items = append(w.items, item)
	}
	rows, err := w.nodeRows(ctx, q, w.items)
	if err != nil {
		return err
	}
	for item, row := range rows {
		node := w.nodes[item]
		node.Type, node.MadeAt = refs.TreeNodeType(item.Kind, row.Supertag, ""), row.MadeAt
		w.nodes[item] = node
	}
	return nil
}

// readSubtrees takes, level by level, the part_of children of every node of frontier and of each
// child it takes, at the walk's cut, taking each node once. It passes over a node of another
// issue, and over an issue's node, and so over everything under either (refs.TreeOwns), and it
// takes a node it does not enter (enters) and nothing under it. Every level is read in one
// statement (refs.TreeSubtrees), so the walk's reads do not grow with the tree's depth; the levels
// are then taken here, in the order a read of each level answers them. above marks each node it
// takes as one of the walk's second part. Every row the read answers is kept under its parent
// (under), the rows of a parent an earlier read answered aside.
func (w *treeWalk) readSubtrees(ctx context.Context, q Queryer, frontier []refs.Target, above bool) error {
	if len(frontier) == 0 {
		return nil
	}
	under, err := w.subtrees(ctx, q, frontier)
	if err != nil {
		return err
	}
	for parent, children := range under {
		if _, held := w.under[parent]; !held {
			w.under[parent] = children
		}
	}
	w.takeLevels(frontier, under, above)
	return nil
}

// takeLevels takes, level by level, the children under holds of every node of frontier the walk
// enters and of each child it takes, each node of the issue once, in under's order; above marks
// each as one of the walk's second part. A walk of the skeleton takes no block of a document it
// does not enter (hiddenBlock) wherever one stands - a check of an upload stands under the
// requirement it serves - since the head rows it reads hold none: what stands under such a block
// it lifts (liftHidden).
func (w *treeWalk) takeLevels(frontier []refs.Target, under map[refs.Target][]refs.TreeChild, above bool) {
	for len(frontier) > 0 {
		next := frontier[:0:0]
		for _, parent := range frontier {
			if !w.enters(parent) {
				continue
			}
			for _, child := range under[parent] {
				if _, seen := w.nodes[child.Node]; seen || !refs.TreeOwns(w.owner, child.Node) || w.hiddenBlock(child.Node) {
					continue
				}
				w.nodes[child.Node] = treeNodeOf(child, above)
				w.children[child.Parent] = append(w.children[child.Parent], child.Node)
				next = append(next, child.Node)
			}
		}
		frontier = next
	}
}

// treeNodeOf is the node child names, as the walk takes it, with the row the children read read
// of it.
func treeNodeOf(child refs.TreeChild, above bool) TreeNode {
	return TreeNode{
		Kind: child.Node.Kind, ID: child.Node.ID, Parent: child.Parent, Type: child.Type,
		Rank: child.Rank, Ranked: child.Ranked, MadeAt: child.MadeAt, Above: above,
		Supertag: child.Supertag, Row: true,
	}
}

// placeItems hangs each conversation item under its parent (WalkIssueTree): the parent its loader
// names where that is a node of the conversation, for a comment written on a document the typed
// block its anchor lies inside, and else the issue. A walk of the skeleton names the parent of a
// comment it hangs under the issue that way where the walk of the whole tree hangs it under that
// parent: a comment of another page of the conversation, or of one of its blocks (offPage), and
// the typed block of a document the walk does not enter that the comment's anchor lies inside
// (nameAnchors).
func (w *treeWalk) placeItems(ctx context.Context, q Queryer) error {
	w.hanging = map[refs.Target][]refs.Target{}
	off, err := w.offPage(ctx, q)
	if err != nil {
		return err
	}
	var anchored []refs.Target
	for _, item := range w.items {
		loaded, held := w.loaded[item]
		if !held {
			continue
		}
		parent, conversation := w.conversationParent(loaded)
		if conversation {
			w.hang(item, parent)
			continue
		}
		if comment := w.pagedComment(parent); comment != "" && off[comment] {
			w.name(item, parent)
		}
		if item.Kind == graphstore.KindComment && loaded != nil && loaded.Kind == graphstore.KindArtifact {
			anchored = append(anchored, item)
			continue
		}
		w.hang(item, w.issue)
	}
	if len(anchored) == 0 {
		return nil
	}
	facts, err := w.facts(ctx, q, anchored)
	if err != nil {
		return err
	}
	addresses := map[refs.Target]string{}
	var unheld []string
	for _, comment := range anchored {
		for _, relation := range facts[comment].Relations {
			if relation.Kind != graphstore.TupleAnchoredIn || relation.To.Kind != refs.BlockNodeKind {
				continue
			}
			addresses[comment] = relation.To.ID
			if _, held := w.nodes[relation.To]; !held {
				unheld = append(unheld, relation.To.ID)
			}
		}
	}
	moved, err := refs.TreeMovedTo(ctx, q, unheld, w.cut)
	if err != nil {
		return err
	}
	unentered := map[refs.Target]refs.Target{}
	for _, comment := range anchored {
		parent := w.anchorParent(addresses[comment], moved)
		w.hang(comment, parent)
		document, _, _ := strings.Cut(addresses[comment], "#")
		if parent == w.issue && addresses[comment] != "" && !w.enters(refs.Target{Kind: graphstore.KindArtifact, ID: document}) {
			unentered[comment] = refs.Target{Kind: refs.BlockNodeKind, ID: addresses[comment]}
		}
	}
	return w.nameAnchors(ctx, q, unentered)
}

// pagedComment is, on a walk of the skeleton, the comment parent is, or whose block it is - a
// parent a loader names that the walk does not take, which may be a comment of another page of
// the conversation - and "" for any other parent and on any other walk.
func (w *treeWalk) pagedComment(parent refs.Target) string {
	if w.skeleton == nil {
		return ""
	}
	switch parent.Kind {
	case graphstore.KindComment:
		return parent.ID
	case refs.ItemComment.BlockKind():
		if _, item, _, ok := refs.ParseItemBlockNode(parent.ID); ok {
			return item
		}
	}
	return ""
}

// offPage is each comment of the conversation on another page than the skeleton's that a loader
// names, itself or one of its blocks, as the parent of an item that does not hang under it
// (pagedComment, refs.CommentsOffPage), read in one statement where a loader names any.
func (w *treeWalk) offPage(ctx context.Context, q Queryer) (map[string]bool, error) {
	var comments []string
	named := map[string]bool{}
	for _, item := range w.items {
		loaded, held := w.loaded[item]
		if !held {
			continue
		}
		parent, conversation := w.conversationParent(loaded)
		if comment := w.pagedComment(parent); !conversation && comment != "" && !named[comment] {
			named[comment] = true
			comments = append(comments, comment)
		}
	}
	if len(comments) == 0 {
		return nil, nil
	}
	return refs.CommentsOffPage(ctx, q, w.issue.ID, w.cut.At, w.skeleton.Comments, comments)
}

// name names parent as the parent of item, which the walk hangs under the issue.
func (w *treeWalk) name(item, parent refs.Target) {
	if w.named == nil {
		w.named = map[refs.Target]refs.Target{}
	}
	w.named[item] = parent
}

// nameAnchors names, for each comment of anchored, which a walk of the skeleton hangs under the
// issue because its anchor, the block it maps the comment to, lies in a document the walk does not
// enter, the typed block the walk of the whole tree hangs the comment under: the anchor's own block
// where it is typed, else its nearest typed ancestor, and none where no block at or above it is
// typed or one of them is gone. It reads the blocks from the anchors up, one level at a time, each
// level's facts and rows in a statement each (refs.TreeFactsOf, refs.TreeNodeRows), as the walk of
// the tuples reads a node's type off its supertag and its text's opening line (refs.TreeNodeType).
func (w *treeWalk) nameAnchors(ctx context.Context, q Queryer, anchored map[refs.Target]refs.Target) error {
	for steps := 0; len(anchored) > 0 && steps <= maxAnchorClimb; steps++ {
		level := make([]refs.Target, 0, len(anchored))
		for _, block := range anchored {
			if !slices.Contains(level, block) {
				level = append(level, block)
			}
		}
		facts, err := refs.TreeFactsOf(ctx, q, level, w.cut)
		if err != nil {
			return err
		}
		rows, err := refs.TreeNodeRows(ctx, q, level)
		if err != nil {
			return err
		}
		for comment, block := range anchored {
			held := facts[block]
			opening, _, _ := strings.Cut(held.Values[graphstore.AttributeText], "\n")
			switch {
			case held.Values[graphstore.AttributeState] == graphstore.StateGone:
				delete(anchored, comment)
				continue
			case pmdoc.IsTypedBlock(refs.TreeNodeType(block.Kind, rows[block].Supertag, opening)):
				w.name(comment, block)
				delete(anchored, comment)
				continue
			}
			above := refs.Target{}
			for _, relation := range held.Relations {
				if relation.Kind == graphstore.TuplePartOf {
					above = relation.To
				}
			}
			if above.Kind != refs.BlockNodeKind {
				delete(anchored, comment)
				continue
			}
			anchored[comment] = above
		}
	}
	return nil
}

// maxAnchorClimb bounds the levels nameAnchors climbs, past any document's depth, so a cycle of
// part_of tuples ends it.
const maxAnchorClimb = 256

// conversationParent is the node a loader's parent names, and whether it is a node of the walk's
// conversation, which only a node the conversation test passes is: an item, by its kind, or a current block of
// one, which a loader names as a block (`<kind>/<uuid>#<block id>`).
func (w *treeWalk) conversationParent(loaded *model.NodeParent) (refs.Target, bool) {
	if loaded == nil {
		return refs.Target{}, false
	}
	if kind, item, _, ok := refs.ParseItemBlockNode(loaded.ID); ok && loaded.Kind == refs.BlockNodeKind {
		block := refs.Target{Kind: kind.BlockKind(), ID: loaded.ID}
		return block, slices.Contains(w.blocks[refs.Target{Kind: string(kind), ID: item}], block)
	}
	parent := refs.Target{Kind: loaded.Kind, ID: loaded.ID}
	_, item := w.loaded[parent]
	return parent, item
}

// anchorParent is the node a comment anchored at the block address hangs under: the nearest typed
// block at or above the node the address names, or the node its moves reach (moved), and the
// issue where none is a node of the walk or none of them is typed.
func (w *treeWalk) anchorParent(address string, moved map[string][]string) refs.Target {
	if address == "" {
		return w.issue
	}
	start := refs.Target{Kind: refs.BlockNodeKind, ID: address}
	if _, held := w.nodes[start]; !held {
		start = refs.Target{}
		for _, reached := range moved[address] {
			if _, held := w.nodes[refs.Target{Kind: refs.BlockNodeKind, ID: reached}]; held {
				start = refs.Target{Kind: refs.BlockNodeKind, ID: reached}
				break
			}
		}
	}
	for at, steps := start, 0; at != (refs.Target{}) && at != w.issue && steps <= len(w.nodes); steps++ {
		node, held := w.nodes[at]
		if !held {
			break
		}
		if node.Kind == refs.BlockNodeKind && pmdoc.IsTypedBlock(node.Type) {
			return at
		}
		at = node.Parent
	}
	return w.issue
}

// hang hangs item under parent.
func (w *treeWalk) hang(item, parent refs.Target) {
	node := w.nodes[item]
	node.Parent = parent
	w.nodes[item] = node
	w.hanging[parent] = append(w.hanging[parent], item)
}

// ordered is the children of parent in the walk's order: a conversation item's own blocks first,
// in their position order; then its ranked children, in the order the children read answered them,
// by rank and then address; then every unranked child and every item that hangs under it, by when
// each was made and then by address.
func (w *treeWalk) ordered(parent refs.Target) []refs.Target {
	children := slices.Clone(w.blocks[parent])
	var unranked []refs.Target
	for _, child := range w.children[parent] {
		if w.nodes[child].Ranked {
			children = append(children, child)
			continue
		}
		unranked = append(unranked, child)
	}
	unranked = append(unranked, w.hanging[parent]...)
	slices.SortStableFunc(unranked, func(a, b refs.Target) int {
		return cmp.Or(w.nodes[a].MadeAt.Compare(w.nodes[b].MadeAt), refs.CompareAddresses(a, b))
	})
	return append(children, unranked...)
}

// take appends node and its subtree to tree in the walk's order, each node once.
func (w *treeWalk) take(tree *IssueTree, node refs.Target) {
	if _, taken := tree.index[node]; taken {
		return
	}
	tree.index[node] = len(tree.Nodes)
	tree.Nodes = append(tree.Nodes, w.nodes[node])
	for _, child := range w.ordered(node) {
		w.take(tree, child)
	}
}

// QueryRecord is the record of one query of an issue at one cut: the query, the nodes it takes in
// the walk's order, the record's bytes, its digest - the lower-case hex sha256 of the bytes - and
// the cut it was read at, whose ReadSeq is the highest graph_tuples.seq it read.
type QueryRecord struct {
	Query  string
	Nodes  []TreeNode
	Record []byte
	Digest string
	Cut    refs.TreeCut
}

// ReadQueryRecord is the record of query of the issue issueKey at cut, the head's where cut is
// zero: the walk of the issue's tree (WalkIssueTree) and the one builder of its record
// (IssueTree.QueryRecord), with JIT off for the rest of the transaction q reads in
// (refs.TurnJITOff).
func ReadQueryRecord(ctx context.Context, q Queryer, issueKey, query string, cut refs.TreeCut) (QueryRecord, error) {
	if _, defined := queryDefinitions[query]; !defined {
		return QueryRecord{}, fmt.Errorf("the query %q has no record", query)
	}
	if err := refs.TurnJITOff(ctx, q); err != nil {
		return QueryRecord{}, err
	}
	tree, err := WalkIssueTree(ctx, q, issueKey, cut)
	if err != nil {
		return QueryRecord{}, err
	}
	return tree.QueryRecord(ctx, q, query)
}

// Takes is the index in the walk of each node the query takes, in the walk's order, by the one
// definition its record is built by (queryDefinitions), or for the decisions query, whose record is
// not built yet, by its rule (decisionsTakes), and false where neither names the query. The tree
// read's view of a query answers these nodes as the query's.
func (tree IssueTree) Takes(query string) ([]int, bool) {
	takes, defined := queryDefinitions[query]
	if !defined && query == graphstore.QueryDecisions {
		takes, defined = decisionsTakes, true
	}
	if !defined {
		return nil, false
	}
	var taken []int
	for index := range tree.Nodes {
		if takes(tree, index) {
			taken = append(taken, index)
		}
	}
	return taken, true
}

// recordLeftOut are the values a record writes no field of among the node's values. It leaves out
// a value only where another field of the record carries it: the rank, written in a field of its
// own, and the text, written as the body, since a value written twice would let two builders write
// two digests. Every other value - the node's visibility, its state, its placement and its
// text_rank among them - is among the values, so a change of any of them moves the digest, and so
// is the text_parent among the relations.
var recordLeftOut = []string{refs.AttributeRank, graphstore.AttributeText}

// QueryRecord is the record of query over the walk: for each node the
// query takes, in the walk's order, seven fields - the node's address, its type, its parent's
// address, its rank, its values, its body text, and the relations out of it - each written as its
// byte length, one space, then its bytes, so no text inside a field can imitate a boundary. The
// values field holds, for each value in its attribute's name order (recordLeftOut aside), the
// attribute's name and the value, each a field; the relations field holds, for each relation by
// its kind and then its far end, the kind and the far end's address, each a field. The values and
// the relations are read at the walk's cut, as the server sees the store.
func (tree IssueTree) QueryRecord(ctx context.Context, q Queryer, query string) (QueryRecord, error) {
	takes, defined := queryDefinitions[query]
	if !defined {
		return QueryRecord{}, fmt.Errorf("the query %q has no record", query)
	}
	var taken []TreeNode
	var targets []refs.Target
	for index, node := range tree.Nodes {
		if takes(tree, index) {
			taken = append(taken, node)
			targets = append(targets, node.target())
		}
	}
	facts, err := refs.TreeFactsOf(ctx, q, targets, tree.Cut)
	if err != nil {
		return QueryRecord{}, fmt.Errorf("read the %s record of %s: %w", query, tree.Issue, err)
	}
	var record, scratch bytes.Buffer
	for _, node := range taken {
		held := facts[node.target()]
		writeRecordField(&record, node.Address())
		writeRecordField(&record, node.Type)
		writeRecordField(&record, node.Parent.Kind+":"+node.Parent.ID)
		writeRecordField(&record, node.Rank)
		scratch.Reset()
		names := make([]string, 0, len(held.Values))
		for name := range held.Values {
			if !slices.Contains(recordLeftOut, name) {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			writeRecordField(&scratch, name)
			writeRecordField(&scratch, held.Values[name])
		}
		writeRecordField(&record, scratch.String())
		writeRecordField(&record, held.Values[graphstore.AttributeText])
		scratch.Reset()
		relations := slices.Clone(held.Relations)
		slices.SortFunc(relations, func(a, b refs.TreeRelation) int {
			return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.To.Kind+":"+a.To.ID, b.To.Kind+":"+b.To.ID))
		})
		for _, relation := range relations {
			writeRecordField(&scratch, relation.Kind)
			writeRecordField(&scratch, relation.To.Kind+":"+relation.To.ID)
		}
		writeRecordField(&record, scratch.String())
	}
	sum := sha256.Sum256(record.Bytes())
	return QueryRecord{Query: query, Nodes: taken, Record: record.Bytes(), Digest: hex.EncodeToString(sum[:]), Cut: tree.Cut}, nil
}

// writeRecordField writes field to record as its byte length, one space, then its bytes.
func writeRecordField(record *bytes.Buffer, field string) {
	record.WriteString(strconv.Itoa(len(field)))
	record.WriteByte(' ')
	record.WriteString(field)
}
