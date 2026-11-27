package docs

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// childrenOf is each child of the block of tree that carries id, of type nodeType, written
// type#id (an untyped paragraph's id included), failing where the tree holds no such block.
func childrenOf(t *testing.T, tree *pmdoc.Node, id, nodeType string) []string {
	t.Helper()
	block := blockWithID(tree, id, nodeType)
	if block == nil {
		t.Fatalf("the document holds no %s %s", nodeType, id)
	}
	held := make([]string, 0, len(block.Children))
	for _, child := range block.Children {
		held = append(held, child.Type+"#"+blockID(child))
	}
	return held
}

// refusalCode is the code the edit route answers err with where it is an operation the route
// refuses as INVALID_OP (api/handler_errors.go), for a failure to print.
func refusalCode(err error) string {
	if invalid := (*ErrInvalidOp)(nil); errors.As(err, &invalid) {
		return "INVALID_OP"
	}
	return "no INVALID_OP"
}

// An insert whose into names a block writes its markdown inside that block, after its last child,
// read as that block's content rather than as a document of its own, so an agent writes a
// reading into a person's answered ask, and into its own decision, with one insert each. The same
// reading written beside the block with after is refused, since a reading at a document's level
// has no parent. into stands alone and names a block that holds others: beside after, on a move,
// or naming a quote, a heading or a document edge it is refused naming into, and into a paragraph
// naming that paragraph. A reading into the ask that holds no answer is refused naming the reading
// and the ask. No refused insert changes the document.
func TestAnInsertIntoABlockStandsLastInsideIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	seedServiceText(t, service, artifactID, "Intro.\n\n"+
		":::ask{#a1 urgency=\"med\" multiple=\"false\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n\n"+
		":::ask{#a2 urgency=\"med\" multiple=\"false\"}\nWhich region first?\n\n- East: The quiet one\n- West: The busy one\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	answerBlockAs(t, service, artifactID, "a1", "alice", []string{"Ship"})
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::decision{#d1}\nWe ship on Friday.\n\nThe release notes say so.\n:::\n",
	}}, agent, nil); err != nil {
		t.Fatalf("the agent writes its decision d1: %v", err)
	}
	insert := func(op model.EditOp) error {
		t.Helper()
		_, err := joinedApplyOps(service, artifactID, []model.EditOp{op}, agent, nil)
		return err
	}
	const answerReading = ":::reading{#r1}\nThe agent reads the answer as shipping this week.\n:::\n"

	// Into the person's answered ask: after its options, the ask's last child.
	if err := insert(model.EditOp{Op: "insert", Into: "block:a1", Markdown: answerReading}); err != nil {
		t.Fatalf("a reading inserted into the answered ask a1 is refused with %s: %v; want it standing last inside the ask", refusalCode(err), err)
	}
	tree := liveTree(t, service, artifactID)
	if held := childrenOf(t, tree, "a1", "ask"); len(held) != 3 || !strings.HasPrefix(held[0], "paragraph#") ||
		!strings.HasPrefix(held[1], "bullet_list#") || held[2] != "reading#r1" {
		t.Fatalf("the answered ask a1 holds %v after the insert, want its question, its options and then the reading r1", held)
	}

	// Into the agent's decision: after its second paragraph, the decision's last child.
	if err := insert(model.EditOp{Op: "insert", Into: "block:d1", Markdown: ":::reading{#r2}\nThe agent reads it as Friday for every service.\n:::\n"}); err != nil {
		t.Fatalf("a reading inserted into the decision d1 is refused with %s: %v; want it standing last inside the decision", refusalCode(err), err)
	}
	tree = liveTree(t, service, artifactID)
	decision := blockWithID(tree, "d1", "decision")
	if held := childrenOf(t, tree, "d1", "decision"); len(held) != 3 || !strings.HasPrefix(held[0], "paragraph#") ||
		pmdoc.BlockText(tree)[blockID(decision.Children[1])] != "The release notes say so." || held[2] != "reading#r2" {
		t.Fatalf("the decision d1 holds %v after the insert, want its two paragraphs and then the reading r2", held)
	}
	markdown := liveMarkdownOf(t, service, artifactID)
	if !strings.Contains(markdown, "The release notes say so.\n\n:::reading{#r2}\nThe agent reads it as Friday for every service.\n:::\n::::\n") ||
		!strings.Contains(markdown, "- Hold: Wait for review\n\n:::reading{#r1}\nThe agent reads the answer as shipping this week.\n:::\n::::\n") {
		t.Fatalf("the document renders as\n%s\nwant each reading closing its block", markdown)
	}

	// Refusals, each of which leaves the document as it was.
	paragraph := blockID(tree.Children[0])
	refused := func(what string, op model.EditOp, wantField string, names ...string) {
		t.Helper()
		before := liveMarkdownOf(t, service, artifactID)
		err := insert(op)
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || invalid.Field != wantField {
			t.Errorf("%s = %v, want it refused INVALID_OP on the field %q", what, err, wantField)
		}
		for _, name := range names {
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s = %v, want the refusal naming %s", what, err, name)
			}
		}
		if after := liveMarkdownOf(t, service, artifactID); after != before {
			t.Errorf("%s changed the document from\n%s\nto\n%s", what, before, after)
		}
	}
	refused("the same reading inserted after the block a1", model.EditOp{Op: "insert", After: "block:a1", Markdown: strings.Replace(answerReading, "#r1", "#r3", 1)},
		"markdown", `typed block "reading" "r3" stands only at the end of a decision or an ask`)
	refused("an insert naming into beside after", model.EditOp{Op: "insert", Into: "block:d1", After: "block:d1", Markdown: answerReading}, "into")
	refused("an insert naming into beside before", model.EditOp{Op: "insert", Into: "block:d1", Before: "start", Markdown: answerReading}, "into")
	for _, anchor := range []string{"We ship on Friday.", "heading:Intro", "start", "end", "block:"} {
		refused("an insert into "+anchor, model.EditOp{Op: "insert", Into: anchor, Markdown: answerReading}, "into", `"`+anchor+`"`)
	}
	refused("a move naming into", model.EditOp{Op: "move", Block: "d1", Into: "block:a1"}, "into")
	refused("an insert into the paragraph "+paragraph, model.EditOp{Op: "insert", Into: "block:" + paragraph, Markdown: "A second line.\n"},
		"into", `block "`+paragraph+`" is a paragraph`, "holds no blocks")
	refused("a paragraph inserted into d1 after its reading", model.EditOp{Op: "insert", Into: "block:d1", Markdown: "And we say so.\n"},
		"markdown", `typed block "reading" "r2" stands only at the end of a decision or an ask`)

	// Into the ask that holds no answer: refused naming the reading and the ask.
	open := blockAskOn(t, service, artifactID, "a2")
	before := liveMarkdownOf(t, service, artifactID)
	err := insert(model.EditOp{Op: "insert", Into: "block:a2", Markdown: ":::reading{#r4}\nThe agent reads it as East first.\n:::\n"})
	var noAnswer *ErrInvalidAskBlock
	if want := `reading "r4" stands in the ask dispatch://` + *open.IssueKey + `/ask/` + open.ID + `, and that ask has no answer yet`; !errors.As(err, &noAnswer) || !strings.Contains(err.Error(), want) {
		t.Errorf("a reading inserted into the open ask a2 = %v, want it refused with %q", err, want)
	}
	if after := liveMarkdownOf(t, service, artifactID); after != before {
		t.Errorf("the refused reading in the open ask changed the document from\n%s\nto\n%s", before, after)
	}
	if held := childrenOf(t, liveTree(t, service, artifactID), "a2", "ask"); slices.ContainsFunc(held, func(child string) bool { return strings.HasPrefix(child, "reading#") }) {
		t.Errorf("the open ask a2 holds %v, want no reading", held)
	}
}
