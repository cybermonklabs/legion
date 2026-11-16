package docs

import (
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// listAnchorFixture is the document every case below anchors in: a bullet list of two items whose
// first item holds a nested list, and a decision after it.
const listAnchorFixture = "- a\n  - x\n  - y\n- b\n\n" + askFixture

// List content an insert or a move anchors inside a list joins that list: beside the item the
// anchor stands in, or as an item of the list whose own block the anchor is. Content that is no
// list content keeps the document-level placement, after the block holding the anchor, so a
// paragraph anchored on a nested item lands after the whole list and one anchored on a block of a
// decision lands beside the decision rather than in it.
func TestApplyOperationListContentJoinsTheAnchorsList(t *testing.T) {
	for _, test := range []struct {
		name string
		op   func(t *testing.T, tree *pmdoc.Node) model.EditOp
		want string
	}{
		{
			name: "an insert quoting a nested item",
			op: func(*testing.T, *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Markdown: "- z", After: "x"}
			},
			want: "- a\n  - x\n  - z\n  - y\n- b\n\n" + askFixture,
		},
		{
			name: "an insert on a nested item's block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Markdown: "- z2", After: "block:" + blockIDOfText(t, tree, "x")}
			},
			want: "- a\n  - x\n  - z2\n  - y\n- b\n\n" + askFixture,
		},
		{
			name: "a move onto a nested item's block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{
					Op:    "move",
					Block: listItemBlockID(t, tree, "b"),
					After: "block:" + blockIDOfText(t, tree, "x"),
				}
			},
			want: "- a\n  - x\n  - b\n  - y\n\n" + askFixture,
		},
		{
			name: "an insert on the list's own block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				listID, _ := tree.Children[0].Attrs[pmdoc.BlockIDAttr].(string)
				return model.EditOp{Op: "insert", Markdown: "- c", After: "block:" + listID}
			},
			want: "- a\n  - x\n  - y\n- b\n- c\n\n" + askFixture,
		},
		{
			name: "an insert of a paragraph on a nested item's block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Markdown: "Plain.", After: "block:" + blockIDOfText(t, tree, "x")}
			},
			want: "- a\n  - x\n  - y\n- b\n\nPlain.\n\n" + askFixture,
		},
		{
			name: "an insert of a paragraph on a block of a decision",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{
					Op:       "insert",
					Markdown: "Plain.",
					After:    "block:" + blockIDOfText(t, tree, "Which transport?"),
				}
			},
			want: "- a\n  - x\n  - y\n- b\n\n" + askFixture + "\nPlain.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(listAnchorFixture)
			if err != nil {
				t.Fatal(err)
			}
			out, err := applyAlone(tree, test.op(t, tree))
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(out)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("after the operation = %q, want %q", markdown, test.want)
			}
		})
	}
}

// A move of a list item anchored outside every list is still the move no list can take: the item
// cannot stand at document level.
func TestApplyOperationMoveOfAListItemOutsideEveryListIsRefused(t *testing.T) {
	tree, err := parseInput(listAnchorFixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyAlone(tree, model.EditOp{
		Op:    "move",
		Block: listItemBlockID(t, tree, "b"),
		After: "Which transport?",
	})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "block" {
		t.Fatalf("move of a list item beside a decision error = %v, want an invalid block", err)
	}
}

// listItemBlockID is the id of the list item whose text is text, the block a move relocates.
func listItemBlockID(t *testing.T, tree *pmdoc.Node, text string) string {
	t.Helper()
	paragraph := blockIDOfText(t, tree, text)
	var found string
	var walk func(node *pmdoc.Node, item string)
	walk = func(node *pmdoc.Node, item string) {
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); node.Type == "list_item" && id != "" {
			item = id
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == paragraph && item != "" {
			found = item
		}
		for _, child := range node.Children {
			walk(child, item)
		}
	}
	walk(tree, "")
	if found == "" {
		t.Fatalf("no list item holds %q", text)
	}
	return found
}
