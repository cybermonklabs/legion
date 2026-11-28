package pmdoc

// DeleteNodeText is d's tree with the text of each block of taken - the ids of blocks of d - taken
// out of it, validated once, or nil where d holds none of them: a copy of the tree, which d keeps as
// it was. It is the text of a delete of the node route (R7 of the issue-tree spec: api's DELETE
// /api/v1/issues/{key}/nodes/{id}), which takes a node and its part_of subtree out of every document
// whose text holds one of them, in one write of each document's tree.
//
// A block the delete does not take that stands in the text of one it takes is lifted out: it stands
// where the taken block's text stood, in its order there, with all of its own text the delete does
// not take, so no block outside what the delete takes leaves a text. The items a taken list holds
// that the delete does not take stay one list of its kind, which the caller gives an id
// (EnsureBlockIDs); a block lifted into a list joins it as a list item (asListItems). A list or a
// quote the delete empties goes with it, and an emptied document or footnote definition keeps one
// empty paragraph, as DeleteBlock leaves one. Any other block the delete does not take and leaves
// with no block of its own - a callout, a list item or a decision whose every block it takes - and a
// list item whose own first paragraph it takes, is a ContainerEmptiedError naming it, and nothing is
// written. No lift-out ever leaves such a block: a block whose text a lift-out empties is one the
// delete takes. The edit route's ops are no part of it: pmdoc.MoveBlock validates the document after
// each move, and the block+ content of a callout or a decision refuses a container left empty for a
// moment. The tree is validated once, at the end.
func (d ValidDocument) DeleteNodeText(taken map[string]bool) (*Node, error) {
	if d.doc == nil {
		return nil, wantsDocument("DeleteNodeText")
	}
	out := cloneNode(d.doc)
	changed, err := deleteTakenText(out, taken)
	if err != nil || !changed {
		return nil, err
	}
	if len(out.Children) == 0 {
		out.Children = []*Node{{Type: "paragraph"}}
	}
	if _, err := ValidateDocument(out, "DeleteNodeText"); err != nil {
		return nil, err
	}
	return out, nil
}

// deleteTakenText takes the text of each block of taken out of parent's content, in place, at
// every depth, lifting out what each holds that the delete does not take (liftedText), and reports
// whether it changed parent. A block it keeps whose content it changed is held to keptText. It
// builds parent's new content only from the first child it changes, so a parent it leaves as it
// was costs no copy of its content.
func deleteTakenText(parent *Node, taken map[string]bool) (bool, error) {
	var children []*Node
	changed := false
	change := func(index int) {
		if !changed {
			changed = true
			children = append(make([]*Node, 0, len(parent.Children)), parent.Children[:index]...)
		}
	}
	inList := isList(parent)
	for index, child := range parent.Children {
		if isInlineNodeType(child.Type) {
			if changed {
				children = append(children, child)
			}
			continue
		}
		if id := blockIDOf(child); id != "" && taken[id] {
			lifted, err := liftedText(child, taken)
			if err != nil {
				return false, err
			}
			if inList {
				lifted = asListItems(lifted)
			}
			change(index)
			children = append(children, lifted...)
			continue
		}
		kept, moved, err := keptText(child, taken)
		if err != nil {
			return false, err
		}
		if moved {
			change(index)
		}
		if kept && changed {
			children = append(children, child)
		}
	}
	if changed {
		parent.Children = children
	}
	return changed, nil
}

// liftedText is what the text of block, which the delete takes, holds that the delete does not
// take, in its order: each block it keeps, with the text of its own the delete takes taken out of
// it (keptText), and what each block it takes holds in turn. The items a taken list keeps stay one
// list of its kind, without the taken list's block id, so the lift-out moves no block id onto a new
// block.
func liftedText(block *Node, taken map[string]bool) ([]*Node, error) {
	var lifted []*Node
	for _, child := range block.Children {
		if isInlineNodeType(child.Type) {
			continue
		}
		if id := blockIDOf(child); id != "" && taken[id] {
			held, err := liftedText(child, taken)
			if err != nil {
				return nil, err
			}
			lifted = append(lifted, held...)
			continue
		}
		kept, _, err := keptText(child, taken)
		if err != nil {
			return nil, err
		}
		if kept {
			lifted = append(lifted, child)
		}
	}
	if len(lifted) > 0 && isList(block) {
		return []*Node{{Type: block.Type, Attrs: listAttrs(block), Children: asListItems(lifted)}}, nil
	}
	return lifted, nil
}

// keptText takes the text of each block of taken out of block, which the delete keeps
// (deleteTakenText), and answers whether block stays in its parent's content and whether it changed.
// A list or a quote the delete empties goes; an emptied footnote definition keeps one empty
// paragraph; any other block it leaves with no block of its own, and a list item whose own first
// paragraph it takes, is a ContainerEmptiedError naming it.
func keptText(block *Node, taken map[string]bool) (kept, changed bool, err error) {
	var first *Node
	if len(block.Children) > 0 {
		first = block.Children[0]
	}
	changed, err = deleteTakenText(block, taken)
	if err != nil || !changed {
		return true, changed, err
	}
	switch block.Type {
	case "bullet_list", "ordered_list", "blockquote":
		return len(block.Children) > 0, true, nil
	case "footnote_definition":
		if len(block.Children) == 0 {
			block.Children = []*Node{{Type: "paragraph"}}
		}
		return true, true, nil
	case "list_item":
		if len(block.Children) == 0 || block.Children[0] != first {
			return false, true, emptied(block)
		}
	}
	if len(block.Children) == 0 {
		return false, true, emptied(block)
	}
	return true, true, nil
}

// asListItems is nodes as the content of a list: a list item as it is, a list's own items, and any
// other block in a new list item of its own, which opens with an empty paragraph where the block is
// no paragraph, since a list item's content is a paragraph followed by blocks.
func asListItems(nodes []*Node) []*Node {
	items := make([]*Node, 0, len(nodes))
	for _, node := range nodes {
		switch node.Type {
		case "list_item":
			items = append(items, node)
		case "bullet_list", "ordered_list":
			items = append(items, node.Children...)
		case "paragraph":
			items = append(items, &Node{Type: "list_item", Children: []*Node{node}})
		default:
			items = append(items, &Node{Type: "list_item", Children: []*Node{{Type: "paragraph"}, node}})
		}
	}
	return items
}
