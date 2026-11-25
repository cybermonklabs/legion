package pmdoc

import (
	"errors"
	"fmt"
	"slices"
)

// NodeTextMove is what a move of the node route (R7 of dispatch://INFRA-1253: api's PATCH
// /api/v1/issues/{key}/nodes/{id}) writes into the tree of the document that holds the block it
// moves: the block's id (Block); whether Markdown can show the block's new place (Nest), and the
// block whose content its text then goes into (Container, "" for the document's top level); the
// blocks of the document that stand under the block's new parent once the move is taken, in their
// order there, Block among them (Siblings), which say where among the container's content its
// text goes; and, where RewriteParent is set, the value a typed block's parent attribute takes
// (Parent, the references it holds, none to take the attribute out).
type NodeTextMove struct {
	Block         string
	Nest          bool
	Container     string
	Siblings      []string
	RewriteParent bool
	Parent        []string
}

// ErrContainerWouldEmpty is a move that would leave a callout or a list item with no block of its
// own: its last block, or a list item's own first paragraph, taken out of it.
var ErrContainerWouldEmpty = errors.New("the move would leave a container with no block of its own")

// ContainerEmptiedError is ErrContainerWouldEmpty for one container: its block id and its type.
type ContainerEmptiedError struct {
	Container, Type string
}

func (e *ContainerEmptiedError) Error() string {
	if e.Type == "list_item" {
		return fmt.Sprintf("the move takes the block that %s %q holds as its own first paragraph out of it, which would leave it no first paragraph of its own",
			blockName(e.Type), e.Container)
	}
	return fmt.Sprintf("the move takes the last block %s %q holds out of it, which would leave it no block of its own", blockName(e.Type), e.Container)
}

func (e *ContainerEmptiedError) Unwrap() error { return ErrContainerWouldEmpty }

// TextPlaceError is a move whose text the schema cannot hold where the move puts it: the tree
// the nesting leaves is outside the schema, as Err says. Block is the moved block's id, Type its
// type, and Container the block whose content the text would join, "" for the document's top
// level. Nothing is written. It is no schema refusal of a stored document (ErrSchema): it does not
// unwrap to Err, so no refusal of a document outside the schema reclassifies it.
type TextPlaceError struct {
	Block, Type, Container string
	Err                    error
}

func (e *TextPlaceError) Error() string {
	where := "the document's top level"
	if e.Container != "" {
		where = fmt.Sprintf("the content of block %q", e.Container)
	}
	return fmt.Sprintf("the text of %s %q cannot stand in %s: %v", blockName(e.Type), e.Block, where, e.Err)
}

// textHolders are the blocks a moved block's text may stand inside and move out of: a callout and
// a list item, which have no words of their own, and the lists and quotes between them. A block
// any other block holds - a decision, a reading, an ask, a requirement, a criterion, a check or a
// plan task, whose content is its own words, a table, a footnote's definition - keeps its text
// where it stands, and so does one under a callout or a list item that stands in such a block.
var textHolders = []string{"callout", "list_item", "bullet_list", "ordered_list", "blockquote"}

// MoveNodeText is d's tree with move written into it, validated, or nil where move changes
// nothing of it: a copy of the tree, which d keeps as it was.
//
// A typed block whose type declares the parent attribute takes Parent as that attribute where
// RewriteParent is set, or loses it where Parent names nothing. Where Nest is set and Markdown can
// show the block's new place, the block's text leaves the content that holds it for Container's:
// the block's text stands in no block but those of textHolders, Container is the document's top
// level or a callout or a list item that stands in none either, and Container is not the block or
// inside it. The text then goes right after the nearest of Siblings before Block that stands in
// Container's content, else right before the nearest after it, else last; in a list item never
// before the item's first paragraph. A list item nests in a list: Container's own list where it is
// a list item that holds one, else a new list of the kind that held the item, which the caller
// gives an id (EnsureBlockIDs). Every list and quote the move empties goes with the block. A
// callout or a list item that the move would leave with no block of its own - its last block, or a
// list item's own first paragraph, taken out of it - is a ContainerEmptiedError naming it, and a
// tree the nesting leaves outside the schema a TextPlaceError naming the block and the container;
// either way nothing is written.
func (d ValidDocument) MoveNodeText(move NodeTextMove) (*Node, error) {
	if d.doc == nil {
		return nil, wantsDocument("MoveNodeText")
	}
	out := cloneNode(d.doc)
	path, ok := blockPath(out, move.Block)
	if !ok {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, move.Block)
	}
	block := nodeAtPath(out, path)
	attributed := writeParentAttribute(block, move)
	if move.Nest {
		nested := cloneNode(out)
		changed, err := nestNodeText(nested, path, move)
		if err != nil {
			return nil, err
		}
		if changed {
			valid, err := ValidateDocument(nested, "MoveNodeText")
			if err != nil {
				return nil, &TextPlaceError{Block: move.Block, Type: block.Type, Container: move.Container, Err: err}
			}
			return valid.doc, nil
		}
	}
	if !attributed {
		return nil, nil
	}
	if _, err := ValidateDocument(out, "MoveNodeText"); err != nil {
		return nil, &TextPlaceError{Block: move.Block, Type: block.Type, Err: err}
	}
	return out, nil
}

// writeParentAttribute writes move's parent attribute into block where move rewrites it and the
// block's type declares one, and reports whether it changed the block.
func writeParentAttribute(block *Node, move NodeTextMove) bool {
	if !move.RewriteParent {
		return false
	}
	typ, typed := typedBlock(block.Type)
	if !typed || !typ.Attributes.Has(ParentAttribute) {
		return false
	}
	held, _ := stringSlice(block.Attrs[ParentAttribute])
	if slices.Equal(held, move.Parent) {
		return false
	}
	if len(move.Parent) == 0 {
		delete(block.Attrs, ParentAttribute)
		return true
	}
	if block.Attrs == nil {
		block.Attrs = Attrs{}
	}
	block.Attrs[ParentAttribute] = slices.Clone(move.Parent)
	return true
}

// nestNodeText moves the text of the block at path of doc into move's Container, in place, and
// reports whether it moved it: false where the block's text or the container stands where no move
// takes or nests a text (textHolders), and where the text already stands where the move puts it.
func nestNodeText(doc *Node, path []int, move NodeTextMove) (bool, error) {
	for depth := 1; depth < len(path); depth++ {
		if !slices.Contains(textHolders, nodeAtPath(doc, path[:depth]).Type) {
			return false, nil
		}
	}
	block := nodeAtPath(doc, path)
	container := doc
	if move.Container != "" {
		at, found := blockPath(doc, move.Container)
		if !found || len(at) >= len(path) && slices.Equal(at[:len(path)], path) {
			return false, nil
		}
		container = nodeAtPath(doc, at)
		if container.Type != "callout" && container.Type != "list_item" {
			return false, nil
		}
		for depth := 1; depth < len(at); depth++ {
			if !slices.Contains(textHolders, nodeAtPath(doc, at[:depth]).Type) {
				return false, nil
			}
		}
	}
	before := cloneNode(doc)
	holder := nodeAtPath(doc, path[:len(path)-1])
	if err := detachBlock(doc, path); err != nil {
		return false, err
	}
	switch {
	case block.Type == "list_item" && container.Type == "list_item":
		list := ownList(container)
		if list == nil {
			list = &Node{Type: holder.Type, Attrs: listAttrs(holder)}
			container.Children = append(container.Children, list)
		}
		insertChild(list, block, siblingIndex(list.Children, move, 0))
	case block.Type == "list_item":
		wrapped := &Node{Type: holder.Type, Attrs: listAttrs(holder), Children: []*Node{block}}
		insertChild(container, wrapped, siblingIndex(container.Children, move, 0))
	case container.Type == "list_item":
		insertChild(container, block, siblingIndex(container.Children, move, 1))
	default:
		insertChild(container, block, siblingIndex(container.Children, move, 0))
	}
	return !doc.EqualWithBlockIDs(before), nil
}

// detachBlock takes the block at path out of doc, with each list and quote that leaves empty, and
// refuses a move that would leave the callout or the list item it stood in with no block of its
// own (ContainerEmptiedError).
func detachBlock(doc *Node, path []int) error {
	for depth := len(path); depth > 0; depth-- {
		parent := nodeAtPath(doc, path[:depth-1])
		index := path[depth-1]
		parent.Children = append(parent.Children[:index:index], parent.Children[index+1:]...)
		switch parent.Type {
		case "bullet_list", "ordered_list", "blockquote":
			if len(parent.Children) == 0 && depth > 1 {
				continue
			}
		case "callout":
			if len(parent.Children) == 0 {
				return emptied(parent)
			}
		case "list_item":
			// The item's own first paragraph is its text: taking it out leaves the item none of
			// its own, whatever follows it.
			if index == 0 || len(parent.Children) == 0 {
				return emptied(parent)
			}
		}
		return nil
	}
	return nil
}

// emptied is the ContainerEmptiedError naming container.
func emptied(container *Node) error {
	id, _ := container.Attrs[BlockIDAttr].(string)
	return &ContainerEmptiedError{Container: id, Type: container.Type}
}

// ownList is the last list a list item holds in its own content, nil where it holds none.
func ownList(item *Node) *Node {
	for index := len(item.Children) - 1; index >= 0; index-- {
		if child := item.Children[index]; child.Type == "bullet_list" || child.Type == "ordered_list" {
			return child
		}
	}
	return nil
}

// listAttrs are the attributes a new list takes from the list a moved item stood in, without its
// block id, which the new list takes of its own.
func listAttrs(list *Node) Attrs {
	attrs := cloneAttrs(list.Attrs)
	delete(attrs, BlockIDAttr)
	return attrs
}

// insertChild puts child into parent's content at index.
func insertChild(parent, child *Node, index int) {
	parent.Children = slices.Insert(parent.Children, index, child)
}

// siblingIndex is where among children the moved block's text goes (MoveNodeText): right after the
// nearest of move's Siblings before Block that children hold, else right before the nearest after
// it, else last; never before least. A child holds a sibling where it is the sibling or a list
// whose own items hold it.
func siblingIndex(children []*Node, move NodeTextMove, least int) int {
	at := slices.Index(move.Siblings, move.Block)
	if at < 0 {
		return max(len(children), least)
	}
	for index := at - 1; index >= 0; index-- {
		if child := holdingChild(children, move.Siblings[index]); child >= 0 {
			return max(child+1, least)
		}
	}
	for index := at + 1; index < len(move.Siblings); index++ {
		if child := holdingChild(children, move.Siblings[index]); child >= 0 {
			return max(child, least)
		}
	}
	return max(len(children), least)
}

// holdingChild is the index among children of the one that is the block id names, or a list whose
// own items hold it, and -1 for none.
func holdingChild(children []*Node, id string) int {
	for index, child := range children {
		if child.Attrs[BlockIDAttr] == id {
			return index
		}
		if child.Type == "bullet_list" || child.Type == "ordered_list" {
			for _, item := range child.Children {
				if item.Attrs[BlockIDAttr] == id {
					return index
				}
			}
		}
	}
	return -1
}

// blockPath is the path in doc of the block carrying id, and false where none does.
func blockPath(doc *Node, id string) ([]int, bool) {
	path, _, found := findBlock(doc, id)
	return path, found
}
