package pmdoc

import "slices"

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
// next's children are paired with live's as Update's updateChildren aligns them (carryChildren), so
// the ids land where Update keeps elements. live is compared with its anchor marks stripped, which
// the markdown never holds, with every untyped block's id masked, and with a typed block's id
// masked too where next names none for it, along with the whole of that block's server state,
// which an upload resets (unchangedAttrs). An id goes only to unchanged content at its paired
// position, never onto other content: a moved, swapped, retyped or edited block, every block
// holding a change, and every block a moved block passes, which the alignment pairs with a changed
// neighbour, still take a fresh id from EnsureBlockIDs. An id next names anywhere is not carried,
// nor is one no ask row or anchor could store (storedBlockID). A repeat live carries may be
// carried to both holders, and EnsureBlockIDs keeps it for the first, as it would have.
func carryBlockIDs(next, live *Node) {
	if next == nil || live == nil {
		return
	}
	carrier := blockIDCarrier{named: blockIDCounts(next)}
	carrier.carryChildren(next.Children, StripAnchorMarks(live).Children)
}

// blockIDCarrier carries live's ids onto next (carryBlockIDs). named counts every id next names
// before any is carried, which no carried id may take.
type blockIDCarrier struct {
	named map[string]int
}

// carryChildren pairs next with live, a block's children in each tree, as updateChildren pairs a
// tree's children with a Yjs fragment's: their common prefix and suffix of unchanged children
// (unchangedEnds), whose ids are carried, and then the middle, where a text run advances past a
// text run, a same-kind element at the left or right end is kept (by the larger child equality
// factor when both are), and a run of children neither end keeps is replaced. A kept pair that is
// unchanged carries its ids; one that changed carries none for itself, and its children are paired
// in turn. What is left unpaired takes fresh ids. The ends are found before any id is carried, and
// an id lands only on a child a pass has already taken, so nothing carried is compared again.
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

	// A factor is kept for the end it was computed at, since the end that is not kept is weighed
	// again, unchanged, on the next pass.
	leftAt, rightAt := -1, -1
	var leftFactor, rightFactor int
	for len(liveChildren)-left-right > 0 && len(nextChildren)-left-right > 0 {
		leftLive, leftNext := liveChildren[left], nextChildren[left]
		rightLive, rightNext := liveChildren[len(liveChildren)-right-1], nextChildren[len(nextChildren)-right-1]
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
			for left+run < len(liveChildren)-right && left+run < len(nextChildren)-right && !sameChildKind(nextChildren[left+run], liveChildren[left+run]) {
				run++
			}
			left += run
		}
	}
}

// carryKept carries ids for a pair of blocks updateChildren keeps, one for the other: all of them
// when the pair is unchanged, and otherwise those of its unchanged descendants. A typed block whose
// id next does not name as live's is another block, however alike, so nothing in it is carried.
func (c blockIDCarrier) carryKept(next, live *Node) {
	if unchangedNode(next, live) {
		c.carryUnchanged(next, live)
		return
	}
	if IsTypedBlock(next.Type) {
		nextID, named := storedBlockID(next)
		if liveID, _ := storedBlockID(live); !named || nextID != liveID {
			return
		}
	}
	c.carryChildren(next.Children, live.Children)
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
// state and the settlement writes it back from the ask row (docs.setAskServerAttributes), and an
// agent composing a document body cannot know the answer a person gave: an answered ask whose
// question text is unchanged must still read as unchanged, or it takes a fresh id, orphans the row
// holding that answer and opens a second row on the same question.
func unchangedAttrs(nodeType string, next, live Attrs) bool {
	if !IsTypedBlock(nodeType) {
		return attrsEqualButBlockID(next, live)
	}
	if id, _ := next[BlockIDAttr].(string); id == "" {
		return attrsEqualOutsideServerStateBut(nodeType, next, live, func(name string) bool {
			return name == BlockIDAttr || IsAnswerAttribute(name)
		})
	}
	return attrsEqualOutsideServerState(nodeType, next, live)
}
