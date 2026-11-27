package pmdoc

import (
	"fmt"
	"slices"
)

// BlockRemoval is the path, the child indexes from the document down, of the outermost node
// DeleteBlockKeeping(blockID, held) removes from the tree d validated with all that node holds:
// the block carrying blockID where blocks inside it that held does not name take its place
// (keptBlocks), and otherwise that block or the list, list item or blockquote its removal empties,
// up through every such container (removalRoot). MoveBlock takes the block it moves out of its
// place by the same removal, so BlockRemoval(blockID, nil) is what a move of blockID removes
// there; a paragraph that is its list item's whole content moves as that item, and the item's
// removal empties the containers the paragraph's does. The zero ValidDocument wants a document.
func (d ValidDocument) BlockRemoval(blockID string, held map[string]struct{}) ([]int, error) {
	if d.doc == nil {
		return nil, wantsDocument("BlockRemoval")
	}
	path, _, ok := findBlock(d.doc, blockID)
	if !ok {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	if block := nodeAtPath(d.doc, path); len(keptBlocks(block, held)) > 0 {
		return path, nil
	}
	return removalRoot(d.doc, path), nil
}

// TextblockRemoval is the path of the outermost node DeleteTextblock(r) removes from the tree d
// validated with all it holds, and nil where it removes none: the textblock whose whole text r
// covers, or the list item that goes with that text, or else the list, list item or blockquote
// that removal empties, up through every such container (removalRoot). A list item whose one
// nested list's items take its place keeps every block it held but its text, so the textblock is
// what it removes. Its error is DeleteTextblock's refusal of r (ErrListItemContent). The zero
// ValidDocument wants a document.
func (d ValidDocument) TextblockRemoval(r Range) ([]int, error) {
	if d.doc == nil {
		return nil, wantsDocument("TextblockRemoval")
	}
	removePath, hoistNested, found, err := textblockRemoval(d.doc, r)
	if err != nil || !found {
		return nil, err
	}
	// Hoisted items take the item's place, so the list that held it keeps them: the item's text
	// goes, and of its nested list only the node that held those items.
	if hoistNested {
		removePath = append(slices.Clip(removePath), 0)
	} else {
		removePath = removalRoot(d.doc, removePath)
	}
	return removePath, nil
}

// UncoveredBlocks is what of the node at path in the tree d validated, the outermost node a
// removal takes there (BlockRemoval, TextblockRemoval), a guard naming the block ids in named
// leaves uncovered, as the listing names blocks (ListedBlocks): nothing where a node from the
// document down to that node, the node included, carries a named id, and otherwise what
// uncoveredIn finds in that node. An empty path, which a removal of nothing answers, takes
// nothing, so it leaves nothing uncovered, and the zero ValidDocument, which holds no tree, leaves
// nothing uncovered either.
func (d ValidDocument) UncoveredBlocks(path []int, named map[string]struct{}) []string {
	if d.doc == nil || len(path) == 0 {
		return nil
	}
	node := d.doc
	for _, index := range path {
		node = node.Children[index]
		if carriesNamedID(node, named) {
			return nil
		}
	}
	return uncoveredIn(node, named)
}

// uncoveredIn is what of node, a block no named block holds, a guard naming the block ids in named
// leaves uncovered: nothing where node is named; for a list, a list item or a quote that holds a
// named block, what each block it holds leaves uncovered, since such a wrapper holds nothing of its
// own but its attributes, which no listed token covers, so one emptied of covered blocks is covered
// too; and otherwise node as the listing names it (ListedBlocks), whose tokens cover all it holds.
func uncoveredIn(node *Node, named map[string]struct{}) []string {
	if carriesNamedID(node, named) {
		return nil
	}
	if !wrapsBlocks(node.Type) || !holdsNamedBlock(node, named) {
		return ListedBlocks(node)
	}
	var uncovered []string
	for _, child := range node.Children {
		uncovered = append(uncovered, uncoveredIn(child, named)...)
	}
	return uncovered
}

// carriesNamedID reports a node carrying a block id among named.
func carriesNamedID(node *Node, named map[string]struct{}) bool {
	id, _ := node.Attrs[BlockIDAttr].(string)
	if id == "" {
		return false
	}
	_, found := named[id]
	return found
}

// holdsNamedBlock reports a node that holds a block carrying an id among named.
func holdsNamedBlock(node *Node, named map[string]struct{}) bool {
	holds := false
	Walk(node, func(inner *Node) bool {
		holds = holds || (inner != node && carriesNamedID(inner, named))
		return !holds
	})
	return holds
}

// ListedBlocks is the ids of the outermost blocks in node, node included, that the document's
// listing names (RenderWithBlockOffsets), in document order: node's own where it carries one and is
// no list item, and otherwise those of the blocks it holds. It is for the node a removal takes
// (BlockRemoval, TextblockRemoval), of which a list item is the one kind the listing never names:
// the paragraph that opens an item stands for it there, so an item's are its paragraph's and those
// of the blocks after it, a nested list or a typed block, whose tokens cover all the blocks the
// item holds. No listed token covers the item's own attributes: a task item's checked mark lies on
// the list_item, outside its paragraph's token.
func ListedBlocks(node *Node) []string {
	if id, _ := node.Attrs[BlockIDAttr].(string); id != "" && node.Type != "list_item" {
		return []string{id}
	}
	var ids []string
	for _, child := range node.Children {
		if !isInlineNodeType(child.Type) {
			ids = append(ids, ListedBlocks(child)...)
		}
	}
	return ids
}
