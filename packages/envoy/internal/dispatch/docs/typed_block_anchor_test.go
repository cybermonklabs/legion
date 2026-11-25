package docs

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// typedBlockAnchorRepro is dispatch://INFRA-4345/artifact/typed-block-anchor-repro-md at version
// 1: a paragraph O at the top level, then a requirement r-a holding paragraphs A and B.
const typedBlockAnchorRepro = "# Repro: an anchor inside a typed block\n\nOutside paragraph O.\n\n:::requirement{#r-a}\nInner paragraph A.\n\nInner paragraph B.\n:::\n"

// An anchor inside a typed block places the blocks there, whatever names it: an insert whose
// before quotes a paragraph of a requirement, and a move whose anchor is that paragraph's id or a
// quote of it, put the blocks in the requirement beside the paragraph; a move of a block already
// just before the requirement moves it in, and changes the document; a move of a paragraph of the
// requirement after a top-level block puts it at the top level; and an insert the requirement's
// content rule cannot hold where its anchor lands is refused, naming why, with the document
// unchanged (INFRA-4345 R13, from typed-block-anchor-repro-md versions 2 and 3, where each of
// these answered Applied and left the blocks at the top level, or answered that it changed
// nothing).
func TestAnAnchorInsideATypedBlockPlacesTheBlocksThere(t *testing.T) {
	t.Parallel()
	const inside = ":::requirement{#r-a}\nInner paragraph A.\n\n"
	for _, test := range []struct {
		name string
		op   func(t *testing.T, tree *pmdoc.Node) model.EditOp
		want string
	}{
		{
			name: "an insert whose before quotes B",
			op: func(*testing.T, *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Before: "Inner paragraph B.", Markdown: "Inserted paragraph Q."}
			},
			want: "# Repro: an anchor inside a typed block\n\nOutside paragraph O.\n\n" + inside + "Inserted paragraph Q.\n\nInner paragraph B.\n:::\n",
		},
		{
			name: "a move of O before block:B",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Outside paragraph O."), Before: "block:" + blockIDOfText(t, tree, "Inner paragraph B.")}
			},
			want: "# Repro: an anchor inside a typed block\n\n" + inside + "Outside paragraph O.\n\nInner paragraph B.\n:::\n",
		},
		{
			name: "a move of O whose before quotes B",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Outside paragraph O."), Before: "Inner paragraph B."}
			},
			want: "# Repro: an anchor inside a typed block\n\n" + inside + "Outside paragraph O.\n\nInner paragraph B.\n:::\n",
		},
		{
			name: "a move of O after block:A, O standing just before the requirement",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Outside paragraph O."), After: "block:" + blockIDOfText(t, tree, "Inner paragraph A.")}
			},
			want: "# Repro: an anchor inside a typed block\n\n" + inside + "Outside paragraph O.\n\nInner paragraph B.\n:::\n",
		},
		{
			name: "a move of B after the heading",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Inner paragraph B."), After: "Repro: an anchor inside a typed block"}
			},
			want: "# Repro: an anchor inside a typed block\n\nInner paragraph B.\n\nOutside paragraph O.\n\n:::requirement{#r-a}\nInner paragraph A.\n:::\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(typedBlockAnchorRepro)
			if err != nil {
				t.Fatal(err)
			}
			next, err := applyAlone(tree, test.op(t, tree))
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(next)
			if err != nil {
				t.Fatal(err)
			}
			if markdown != test.want {
				t.Fatalf("the document reads\n%q\nwant\n%q", markdown, test.want)
			}
		})
	}

	t.Run("what the requirement cannot hold is refused", func(t *testing.T) {
		for _, test := range []struct {
			name string
			op   func(t *testing.T, tree *pmdoc.Node) model.EditOp
		}{
			{name: "an insert of a heading whose before quotes B", op: func(*testing.T, *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Before: "Inner paragraph B.", Markdown: "## A heading"}
			}},
			{name: "a move of the heading before block:B", op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Repro: an anchor inside a typed block"), Before: "block:" + blockIDOfText(t, tree, "Inner paragraph B.")}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				tree, err := parseInput(typedBlockAnchorRepro)
				if err != nil {
					t.Fatal(err)
				}
				_, err = applyAlone(tree, test.op(t, tree))
				// The refusal's type is held at the route (INSERT_DOES_NOT_FIT, INVALID_OP); here it
				// is held by what it says, so this file reads the same at main, where the op is
				// written outside the requirement and answers no error at all.
				if err == nil {
					t.Fatalf("the op answered no error, want a refusal")
				}
				t.Logf("refusal: %s", err)
				if !strings.Contains(err.Error(), "a heading") || !strings.Contains(err.Error(), "paragraph+") {
					t.Fatalf("the refusal %q names no rule and no heading", err)
				}
				if markdown, err := renderTree(tree); err != nil || markdown != typedBlockAnchorRepro {
					t.Fatalf("the document reads %q (%v), want it unchanged", markdown, err)
				}
			})
		}
	})

	t.Run("an insert of a table into a check's body is refused", func(t *testing.T) {
		const check = ":::check{#c-1 run=\"r\" when=\"before_merge\"}\nThe check body.\n:::\n"
		tree, err := parseInput(check)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyAlone(tree, model.EditOp{Op: "insert", Before: "The check body.", Markdown: "| a |\n| - |\n| b |"})
		if err == nil || !strings.Contains(err.Error(), "a table") || !strings.Contains(err.Error(), "paragraph+") {
			t.Fatalf("the insert answered %v, want a refusal naming the table and the check's rule", err)
		}
		if markdown, err := renderTree(tree); err != nil || markdown != check {
			t.Fatalf("the document reads %q (%v), want it unchanged", markdown, err)
		}
	})
}
