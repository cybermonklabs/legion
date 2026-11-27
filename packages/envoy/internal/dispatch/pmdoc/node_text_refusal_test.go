package pmdoc

import (
	"errors"
	"strings"
	"testing"
)

// A write of a node's text that would leave a block with no block of its own - a delete
// (DeleteNodeText) or a move (MoveNodeText) of a callout's last block, or of a list item's own
// first paragraph - is refused with a ContainerEmptiedError naming that block and its type, which
// is ErrContainerWouldEmpty. The node route's delete and its move each answer the error's words
// inside a 409 CONTAINER_WOULD_EMPTY that names its own operation, so the words name the block and
// what taking its block out would leave it, and no operation: words that named the move would tell
// a person who deleted a node that a move was refused.
func TestAContainerEmptiedRefusalNamesNoOperation(t *testing.T) {
	t.Parallel()
	paragraph := func(id, text string) *Node {
		return &Node{Type: "paragraph", Attrs: Attrs{BlockIDAttr: id}, Children: []*Node{{Type: "text", Text: text}}}
	}
	doc := &Node{Type: "doc", Children: []*Node{
		{Type: "callout", Attrs: Attrs{"kind": "note", "title": "", BlockIDAttr: "note"}, Children: []*Node{paragraph("inside", "Inside.")}},
		{Type: "bullet_list", Attrs: Attrs{BlockIDAttr: "list"}, Children: []*Node{
			{Type: "list_item", Attrs: Attrs{BlockIDAttr: "item"}, Children: []*Node{paragraph("first", "First."), paragraph("second", "Second.")}},
		}},
		paragraph("after", "After."),
	}}
	valid, err := ValidateDocument(doc, "TestAContainerEmptiedRefusalNamesNoOperation")
	if err != nil {
		t.Fatalf("the fixture is outside the schema: %v", err)
	}
	for _, refused := range []struct {
		name, container, containerType, emptied string
		write                                   func() (*Node, error)
	}{
		{"the delete of a callout's last block", "note", "callout", "no block of its own", func() (*Node, error) {
			return valid.DeleteNodeText(map[string]bool{"inside": true})
		}},
		{"the move of a callout's last block", "note", "callout", "no block of its own", func() (*Node, error) {
			return valid.MoveNodeText(NodeTextMove{Block: "inside", Nest: true, Siblings: []string{"inside", "after"}})
		}},
		{"the delete of a list item's own first paragraph", "item", "list_item", "no first paragraph of its own", func() (*Node, error) {
			return valid.DeleteNodeText(map[string]bool{"first": true})
		}},
		{"the move of a list item's own first paragraph", "item", "list_item", "no first paragraph of its own", func() (*Node, error) {
			return valid.MoveNodeText(NodeTextMove{Block: "first", Nest: true, Siblings: []string{"first", "after"}})
		}},
	} {
		written, err := refused.write()
		var emptied *ContainerEmptiedError
		if written != nil || !errors.As(err, &emptied) || !errors.Is(err, ErrContainerWouldEmpty) ||
			emptied.Container != refused.container || emptied.Type != refused.containerType {
			t.Errorf("%s answered a tree %t and %v, want no tree and a ContainerEmptiedError naming %s %q", refused.name, written != nil, err, refused.containerType, refused.container)
			continue
		}
		words := err.Error()
		if strings.Contains(words, "move") || strings.Contains(words, "delete") ||
			!strings.Contains(words, `"`+refused.container+`"`) || !strings.Contains(words, refused.emptied) {
			t.Errorf("%s is refused in the words %q, want words that name %q and %q and no operation", refused.name, words, refused.container, refused.emptied)
		}
	}
}
