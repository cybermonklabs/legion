package pmdoc

import (
	"hash/maphash"
	"slices"
	"sort"
)

// carryBlockIDs gives each block of next, a whole document's markdown parsed over live, the id of
// the live block it leaves unchanged, where next names no id of its own for it. The markdown names
// no untyped block's id (GET /text renders none), so without this every such block took a fresh
// id, and Update wrote a blockId tombstone for each block of the document on every upload. A typed
// block's markdown names its id when it was rendered from the document, and that id is then part
// of what the block is; a body an agent composed names none, and an ask that took a fresh id there
// orphaned the ask row holding a person's answer and opened a second row on the same question. An
// untouched block keeping its id is what keeps an upload from writing those, and keeps an anchor
// or a reference keyed on the block on it.
//
// Each parent's children in next are aligned with that parent's children in live (carryChildren):
// an unchanged child takes live's id wherever it stands among them, so a version that moves one
// block keeps the id of that block and of every block the move passes (this issue's spec R13),
// and a pointer or an anchor into any of them still resolves. live is compared with its anchor
// marks stripped, which the markdown never holds, with every untyped block's id masked, and with a
// typed block's id masked too where next names none for it, along with the whole of that block's
// server state, which an upload resets (unchangedAttrs). An untyped container the alignment keeps
// in its place though its contents changed keeps its id as well, since its identity is that place
// rather than what it holds (carryKept). Nothing else is carried: every other changed block, a
// leaf included - an edited or retyped paragraph, heading, code block or frontmatter - a changed
// typed block whose markdown names no id, a block put under another parent, and every block the
// alignment pairs with nothing still take a fresh id from EnsureBlockIDs. An id next names
// anywhere is not carried, nor is one no ask row or anchor could store (storedBlockID). A repeat
// live carries may be carried to both holders, and EnsureBlockIDs keeps it for the first, as it
// would have.
func carryBlockIDs(next, live *Node) {
	if next == nil || live == nil {
		return
	}
	carrier := blockIDCarrier{named: blockIDCounts(next), seed: maphash.MakeSeed()}
	carrier.carryChildren(next.Children, StripAnchorMarks(live).Children)
}

// blockIDCarrier carries live's ids onto next (carryBlockIDs). named counts every id next names
// before any is carried, which no carried id may take, and seed is the seed of every content key
// one carry compares (contentKey).
type blockIDCarrier struct {
	named map[string]int
	seed  maphash.Seed
}

// childPair is a child of next and the child of live it leaves unchanged, by their indexes among
// the children carryChildren aligns.
type childPair struct {
	next, live int
}

// carryChildren aligns next with live, a block's children in each tree. Their common prefix and
// suffix of unchanged children (unchangedEnds) carry their ids first. Between those ends, each
// child of next is paired with a child of live it leaves unchanged, wherever that child stands
// (unchangedPairs), and carries its ids: the longest run of those pairs that keeps live's order
// (inOrder) is what the version left in place, and every other pair is a block the version moved
// without changing it. The children no pair holds are paired between each two neighbouring pairs
// of that run as updateChildren pairs the children it rewrites (carryKeptRun), so a changed
// container keeps its id only in its own place, and a child the version moved past a child left
// in place is no pair for one that stood on the other side of it. An id lands only on a child a
// pass has already taken, so nothing carried is compared again.
func (c blockIDCarrier) carryChildren(next, live []*Node) {
	nextChildren := normalizePNodeContent(next)
	liveChildren := normalizePNodeContent(live)
	left, right := unchangedEnds(nextChildren, liveChildren)
	for index := range left {
		c.carryUnchangedChild(nextChildren[index], liveChildren[index])
	}
	for index := 1; index <= right; index++ {
		c.carryUnchangedChild(nextChildren[len(nextChildren)-index], liveChildren[len(liveChildren)-index])
	}
	nextMiddle := nextChildren[left : len(nextChildren)-right]
	liveMiddle := liveChildren[left : len(liveChildren)-right]
	if len(nextMiddle) == 0 || len(liveMiddle) == 0 {
		return
	}
	// One child left on each side is a pair unchangedEnds has found changed already, so no pair
	// of unchangedPairs holds it: it goes to carryKeptRun alone, with no content key read.
	if len(nextMiddle) == 1 && len(liveMiddle) == 1 {
		c.carryKeptRun(nextMiddle, liveMiddle)
		return
	}

	pairs := c.unchangedPairs(nextMiddle, liveMiddle)
	nextPaired := make([]bool, len(nextMiddle))
	livePaired := make([]bool, len(liveMiddle))
	for _, pair := range pairs {
		c.carryUnchanged(nextMiddle[pair.next].element, liveMiddle[pair.live].element)
		nextPaired[pair.next], livePaired[pair.live] = true, true
	}
	nextAt, liveAt := 0, 0
	for _, bound := range append(inOrder(pairs), childPair{next: len(nextMiddle), live: len(liveMiddle)}) {
		c.carryKeptRun(unpairedChildren(nextMiddle[nextAt:bound.next], nextPaired[nextAt:bound.next]),
			unpairedChildren(liveMiddle[liveAt:bound.live], livePaired[liveAt:bound.live]))
		nextAt, liveAt = bound.next+1, bound.live+1
	}
}

// unchangedPairs pairs each element of next with an element of live it leaves unchanged
// (unchangedNode), wherever each stands among its siblings, and answers the pairs in next's order.
// The elements of live are found by their content key (contentKey) and the one a key finds is
// compared whole before it is paired, so two blocks that merely share a key are never paired.
// Elements of one content pair in their order, the first of next's with the first of live's, and
// each element pairs once. A text run pairs with nothing: it holds no block id.
func (c blockIDCarrier) unchangedPairs(next, live []normalizedChild) []childPair {
	holders := make(map[uint64][]int, len(live))
	for index, child := range live {
		if !child.isText() {
			key := c.contentKey(child.element)
			holders[key] = append(holders[key], index)
		}
	}
	taken := make([]bool, len(live))
	var pairs []childPair
	for index, child := range next {
		if child.isText() {
			continue
		}
		key := c.contentKey(child.element)
		candidates := holders[key]
		for _, candidate := range candidates {
			if taken[candidate] || !unchangedNode(child.element, live[candidate].element) {
				continue
			}
			taken[candidate] = true
			pairs = append(pairs, childPair{next: index, live: candidate})
			break
		}
		// The holders a pair took are dropped from the front of the key's list, so a run of
		// repeated content is not walked again from its start for each of its holders.
		kept := candidates
		for len(kept) > 0 && taken[kept[0]] {
			kept = kept[1:]
		}
		if len(kept) != len(candidates) {
			holders[key] = kept
		}
	}
	return pairs
}

// contentKey is a hash of what node holds that unchangedNode compares under every attribute rule
// (unchangedAttrs): its type, its text, the types of its marks, and the same of each of its
// children in order. No attribute enters it, since which of them a comparison weighs depends on
// the block and on what next names, so two blocks unchangedNode holds equal always share a key.
func (c blockIDCarrier) contentKey(node *Node) uint64 {
	var hash maphash.Hash
	hash.SetSeed(c.seed)
	writeContentKey(&hash, node)
	return hash.Sum64()
}

// writeContentKey writes node's part of a content key (contentKey) into hash. A nil child, which
// equalNode holds equal only to another, writes a byte of its own.
func writeContentKey(hash *maphash.Hash, node *Node) {
	if node == nil {
		hash.WriteByte(3)
		return
	}
	hash.WriteString(node.Type)
	hash.WriteByte(0)
	hash.WriteString(node.Text)
	hash.WriteByte(0)
	for _, mark := range node.Marks {
		hash.WriteString(mark.Type)
		hash.WriteByte(0)
	}
	for _, child := range node.Children {
		hash.WriteByte(1)
		writeContentKey(hash, child)
	}
	hash.WriteByte(2)
}

// inOrder is the longest run of pairs, which stand in next's order, whose children of live stand
// in live's order too: the children the version left in place, every other pair being one it
// moved. Where two runs are as long, it is the one whose children of live stand first. The run
// has room for one pair more, the bound carryChildren appends to it.
func inOrder(pairs []childPair) []childPair {
	if len(pairs) == 0 {
		return nil
	}
	// tails[length] is the index in pairs of the pair that ends the run of length+1 pairs whose
	// last child of live stands first, and previous[index] the pair before pairs[index] in the run
	// it ends, -1 for none.
	tails := make([]int, 0, len(pairs))
	previous := make([]int, len(pairs))
	for index, pair := range pairs {
		length := sort.Search(len(tails), func(at int) bool { return pairs[tails[at]].live >= pair.live })
		previous[index] = -1
		if length > 0 {
			previous[index] = tails[length-1]
		}
		if length == len(tails) {
			tails = append(tails, index)
		} else {
			tails[length] = index
		}
	}
	run := make([]childPair, len(tails), len(tails)+1)
	for at, index := len(tails)-1, tails[len(tails)-1]; at >= 0; at, index = at-1, previous[index] {
		run[at] = pairs[index]
	}
	return run
}

// unpairedChildren is the children paired does not mark, in their order: children itself where it
// marks none.
func unpairedChildren(children []normalizedChild, paired []bool) []normalizedChild {
	if !slices.Contains(paired, true) {
		return children
	}
	unpaired := make([]normalizedChild, 0, len(children))
	for index, child := range children {
		if !paired[index] {
			unpaired = append(unpaired, child)
		}
	}
	return unpaired
}

// carryKeptRun pairs next with live, the children of one parent that stand between the same two
// pairs the alignment left in place and that no pair holds, as updateChildren pairs the children
// it rewrites: a text run advances past a text run, a same-kind element at the left or right end
// is kept (by the larger child equality factor when both are), and a run of children neither end
// keeps is replaced. A kept pair carries its own id where carryKept's rule carries it, and its
// children are aligned in turn. What is left unpaired takes fresh ids. No element here leaves
// another unchanged, since unchangedPairs would have paired the two.
func (c blockIDCarrier) carryKeptRun(next, live []normalizedChild) {
	left, right := 0, 0
	// A factor is kept for the end it was computed at, since the end that is not kept is weighed
	// again, unchanged, on the next pass.
	leftAt, rightAt := -1, -1
	var leftFactor, rightFactor int
	for len(live)-left-right > 0 && len(next)-left-right > 0 {
		leftLive, leftNext := live[left], next[left]
		rightLive, rightNext := live[len(live)-right-1], next[len(next)-right-1]
		if leftLive.isText() && leftNext.isText() {
			left++
			continue
		}

		leftMatches := !leftLive.isText() && sameChildKind(leftNext, leftLive)
		rightMatches := !rightLive.isText() && sameChildKind(rightNext, rightLive)
		if leftMatches && rightMatches {
			if leftAt != left {
				leftFactor, leftAt = childEqualityFactor(leftNext.element, leftLive.element), left
			}
			if rightAt != right {
				rightFactor, rightAt = childEqualityFactor(rightNext.element, rightLive.element), right
			}
			if leftFactor < rightFactor {
				leftMatches = false
			} else {
				rightMatches = false
			}
		}

		switch {
		case leftMatches:
			c.carryKept(leftNext.element, leftLive.element)
			left++
		case rightMatches:
			c.carryKept(rightNext.element, rightLive.element)
			right++
		default:
			run := 1
			for left+run < len(live)-right && left+run < len(next)-right && !sameChildKind(next[left+run], live[left+run]) {
				run++
			}
			left += run
		}
	}
}

// carryKept carries ids for a pair of blocks carryKeptRun keeps, one for the other, which is
// never unchanged (unchangedPairs pairs every unchanged block): the block's own id where the rule
// below carries it, and the ids the alignment of its children carries.
//
// An untyped container (isUntypedContainer) keeps its id though its contents changed, because its
// identity is its place in the tree and not what it holds: a list that gains, loses or alters an
// item is still that list, so carrying its id states what is true, where a fresh one would
// assert that a new list exists where none does and reparent every untouched sibling onto it.
// That place is what the call site has established, so it is not weighed again here:
// carryKeptRun comes here only for the pair it keeps at one place among one parent's children,
// between the same two children the alignment left in place, whose kinds match (sameChildKind),
// and a container it pairs with nothing - another kind in that place, one moved past a child left
// in place, one under another parent, one live never held - reaches EnsureBlockIDs instead and is
// minted a fresh id there.
//
// Every other changed untyped block takes a fresh id, a leaf included, as the spec has it: "A
// changed leaf still takes a fresh id". A paragraph, a heading, a code block or a frontmatter
// block is its text, so one whose text changed is other content, and carrying the id onto it
// would move an anchor or a reference keyed on what live said onto content that says something
// else. A kind isUntypedContainer does not name takes a fresh id as well, a kind the schema adds
// later included: at worst a fresh id leaves an anchor keyed on live's block naming a block the
// document no longer holds, where a carried one can move it onto content it never named. A typed
// block whose id next does not name as live's is another block, however alike, so nothing in it
// is carried; the schema owns its identity, and the attributes it carries say what it is. An
// unchanged typed block never comes here: it takes live's id whether it names one or not, as
// every unchanged block does.
func (c blockIDCarrier) carryKept(next, live *Node) {
	if IsTypedBlock(next.Type) {
		nextID, named := storedBlockID(next)
		if liveID, _ := storedBlockID(live); !named || nextID != liveID {
			return
		}
	} else if isUntypedContainer(next.Type) {
		c.carryID(next, live)
	}
	c.carryChildren(next.Children, live.Children)
}

// isUntypedContainer reports whether nodeType is an untyped block whose identity is its place in
// the tree and not what it holds, the one kind of changed untyped block carryKept gives live's
// id. Each holds blocks and no text of its own, so an edit inside it changes what it holds and
// not which block it is: a bullet or an ordered list is the run of items at its place, and a list
// item, a task item among them (a list item whose checked attribute is set), is one place in that
// run; a blockquote quotes the blocks it holds; a footnote definition holds the blocks of the
// note at its place; a table is the grid at its place, and a header row, a row, a header cell and
// a cell are one row or one cell of that grid, a cell holding the one paragraph written there.
// Nothing else is on the list: a textblock and a frontmatter block are their text, an hr has
// neither content nor attributes to change, and html, an image, a hard break and a footnote
// reference are inline atoms, which hold no block id (carryID).
func isUntypedContainer(nodeType string) bool {
	switch nodeType {
	case "bullet_list", "ordered_list", "list_item", "blockquote", "footnote_definition",
		"table", "table_header_row", "table_row", "table_header", "table_cell":
		return true
	}
	return false
}

// carryUnchangedChild carries the ids of a pair of unchanged children: none for a text run, and
// every block's that takes one for an element.
func (c blockIDCarrier) carryUnchangedChild(next, live normalizedChild) {
	if !next.isText() {
		c.carryUnchanged(next.element, live.element)
	}
}

// carryUnchanged gives each block of next, which is unchanged from live (unchangedNode) and so has
// its shape, the id of the block at its place in live (carryID).
func (c blockIDCarrier) carryUnchanged(next, live *Node) {
	if next.Type == "text" {
		return
	}
	c.carryID(next, live)
	for index, child := range next.Children {
		c.carryUnchanged(child, live.Children[index])
	}
}

// carryID gives next, a node of an unchanged pair, live's id when next names no id of its own,
// unless next names live's id elsewhere or no ask row or anchor could store it. A block whose
// markdown names an id, which a typed block's does when it was rendered from the document, keeps
// the one it names. A doc node and an inline node hold no block id at all.
func (c blockIDCarrier) carryID(next, live *Node) {
	if next.Type == "doc" || isInlineNodeType(next.Type) {
		return
	}
	if _, has := storedBlockID(next); has {
		return
	}
	id, storable := storedBlockID(live)
	if !storable || c.named[id] > 0 {
		return
	}
	if next.Attrs == nil {
		next.Attrs = Attrs{}
	}
	next.Attrs[BlockIDAttr] = id
}

// childEqualityFactor is computeChildEqualityFactor over two trees: how many of next's and live's
// children, normalized as updateChildren reads them, are unchanged in their common prefix and
// suffix.
func childEqualityFactor(next, live *Node) int {
	left, right := unchangedEnds(normalizePNodeContent(next.Children), normalizePNodeContent(live.Children))
	return left + right
}

// unchangedEnds is the common prefix and suffix updateChildren keeps of next's and live's
// children, normalized as it reads them: left unchanged children at the start, then right at the
// end of the rest.
func unchangedEnds(next, live []normalizedChild) (left, right int) {
	limit := min(len(live), len(next))
	for left < limit && unchangedChild(next[left], live[left]) {
		left++
	}
	for left+right < limit && unchangedChild(next[len(next)-right-1], live[len(live)-right-1]) {
		right++
	}
	return left, right
}

// sameChildKind is sameKind over two trees: a text run for a text run, or an element of the same
// type.
func sameChildKind(next, live normalizedChild) bool {
	if next.isText() || live.isText() {
		return next.isText() && live.isText()
	}
	return next.element.Type == live.element.Type
}

// unchangedChild is equalYChildPChild over two trees, holding attributes to unchangedAttrs.
func unchangedChild(next, live normalizedChild) bool {
	if next.isText() != live.isText() {
		return false
	}
	if !next.isText() {
		return unchangedNode(next.element, live.element)
	}
	return slices.EqualFunc(next.text, live.text, unchangedNode)
}

// unchangedNode reports whether next leaves live as it is, but for the ids a block may be given
// and a typed block's server attributes (unchangedAttrs). A text run equal node for node is equal
// as a normalized run, so the children need no normalizing to be compared.
func unchangedNode(next, live *Node) bool {
	return equalNode(next, live, unchangedAttrs)
}

// unchangedAttrs compares a block's attributes in next and live: an untyped block's but for its
// id, which is what is carried; a typed block's whose markdown names an id, the one that markdown
// wrote and so part of what the block is, but for the server attributes an upload resets, the
// answer itself apart (attrsEqualOutsideServerState); and a typed block's that names no id but for
// its id, which is then carried like an untyped block's, and the whole of its server state, the
// answer included. The answer is held apart there because an upload resets a typed block's server
// state and the settlement writes back, from the ask's node, what the stored bytes keep of it
// (docs.rollWindowAskAttributes while another build serves, docs.keptAskState once one does), and an
// agent composing a document body cannot know the answer a person gave: an answered ask whose
// question text is unchanged must still read as unchanged, or it takes a fresh id, orphans the row
// holding that answer and opens a second row on the same question.
func unchangedAttrs(nodeType string, next, live Attrs) bool {
	if !IsTypedBlock(nodeType) {
		return attrsEqualButBlockID(next, live)
	}
	if id, _ := next[BlockIDAttr].(string); id == "" {
		typ, _ := typedBlock(nodeType)
		return attrsEqualBut(next, live, func(name string) bool {
			return name == BlockIDAttr || typ.Attributes.Get(name).Server
		})
	}
	return attrsEqualOutsideServerState(nodeType, next, live)
}
