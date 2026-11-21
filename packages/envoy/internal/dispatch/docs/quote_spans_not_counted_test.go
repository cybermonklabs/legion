package docs

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The shapes of dispatch://INFRA-4015/artifact/plan-md that dispatch://INFRA-4345 reports, built
// here rather than read: at version 50 `v46). The` is in one paragraph ("…(the gate on v46). The
// workflows…") and runs across the join of two items of one bullet list, an item ending "(the gate
// on v46)." and the next starting "The errors"; at version 51 the paragraph no longer holds it, so
// its only match is that join. `(the gate on v46).` is inside three blocks of each.
const (
	planAtVersion50 = "Release plan for the gate.\n\n" +
		"The release waits on its check (the gate on v46). The workflows run after it.\n\n" +
		"- The first check passes (the gate on v46).\n- The errors are read last.\n\n" +
		"Nothing ships before (the gate on v46).\n"
	planAtVersion51 = "Release plan for the gate.\n\n" +
		"The release waits on its check (the gate on v46). After it, the workflows run.\n\n" +
		"- The first check passes (the gate on v46).\n- The errors are read last.\n\n" +
		"Nothing ships before (the gate on v46).\n"
	// planLists is the two list items both versions hold, which no row below changes.
	planLists = "- The first check passes (the gate on v46).\n- The errors are read last.\n"
)

// A quote-anchored edit - a replace's find, a delete's find, an insert's anchor - whose quote has
// one match inside one block applies to that match, whatever matches it also has across two or
// more blocks: no edit can take those, so they count toward neither the quote's matches nor its
// ambiguity. At version 50 each was refused TARGET_AMBIGUOUS, `"v46). The" matches 2 places`,
// the second place the join of the two list items (dispatch://INFRA-4345, R1).
func TestAQuoteWithOneMatchInsideABlockAppliesWhateverSpansItAlsoHas(t *testing.T) {
	for _, test := range []struct {
		name string
		op   model.EditOp
		want string
	}{
		{
			name: "a replace",
			op:   model.EditOp{Op: "replace", Find: "v46). The", With: "v46). Then the"},
			want: "Release plan for the gate.\n\n" +
				"The release waits on its check (the gate on v46). Then the workflows run after it.\n\n" +
				planLists + "\nNothing ships before (the gate on v46).\n",
		},
		{
			name: "a delete",
			op:   model.EditOp{Op: "delete", Find: " (the gate on v46). The"},
			want: "Release plan for the gate.\n\n" +
				"The release waits on its check workflows run after it.\n\n" +
				planLists + "\nNothing ships before (the gate on v46).\n",
		},
		{
			name: "an insert anchored on the quote",
			op:   model.EditOp{Op: "insert", After: "v46). The", Markdown: "The gate opens at noon."},
			want: "Release plan for the gate.\n\n" +
				"The release waits on its check (the gate on v46). The workflows run after it.\n\n" +
				"The gate opens at noon.\n\n" +
				planLists + "\nNothing ships before (the gate on v46).\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(planAtVersion50)
			if err != nil {
				t.Fatal(err)
			}
			out, err := applyAlone(tree, test.op)
			if err != nil {
				t.Fatalf("%s at version 50 = %v, want it applied to the paragraph's one match", test.op.Op, err)
			}
			if markdown, err := renderTree(out); err != nil || markdown != test.want {
				t.Fatalf("after the %s = %q, %v; want %q", test.op.Op, markdown, err, test.want)
			}
		})
	}
}

// A quote whose only matches run across blocks is refused as spanning blocks, as it was: at
// version 51 neither `v46). The` nor `v46). The errors` is inside one block.
func TestAQuoteWhoseOnlyMatchesSpanBlocksIsRefusedAsSpanningThem(t *testing.T) {
	tree, err := parseInput(planAtVersion51)
	if err != nil {
		t.Fatal(err)
	}
	occurrence := 0
	for _, op := range []model.EditOp{
		{Op: "replace", Find: "v46). The", With: "v46). The"},
		{Op: "replace", Find: "v46). The errors", With: "v46). The errors"},
		{Op: "delete", Find: "v46). The"},
		{Op: "insert", After: "v46). The", Markdown: "New."},
		// An occurrence counts the matches inside blocks too, and this quote has none.
		{Op: "replace", Find: "v46). The", With: "x", Occurrence: &occurrence},
	} {
		_, err := applyOperations(tree, []model.EditOp{op})
		var spans *ErrQuoteSpansBlocks
		quote := op.Find + op.After
		if !errors.As(err, &spans) || !errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
			t.Fatalf("%s of %q at version 51 = %v, want TARGET_SPANS_BLOCKS", op.Op, quote, err)
		}
		if want := `operation 0: quote "` + quote + `" spans more than one block`; !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("refusal = %q, want it to start %q", err, want)
		}
	}
}

// A quote with two or more matches inside blocks is refused TARGET_AMBIGUOUS naming those
// matches alone, and the count the refusal says is theirs: the control, `(the gate on v46).`, is
// inside three blocks of each version; `v46). The` with a second paragraph holding it is inside
// two, besides the list's join; and an occurrence counts the matches inside blocks, so the
// paragraph's match is occurrence 0 and there is no occurrence 1.
func TestAQuoteWithSeveralMatchesInsideBlocksNamesThoseAlone(t *testing.T) {
	twoInside := planAtVersion50 + "\nAgain the gate holds (the gate on v46). The end.\n"
	for _, test := range []struct {
		name, markdown, quote string
		count                 int
	}{
		{name: "the control at version 50", markdown: planAtVersion50, quote: "(the gate on v46).", count: 3},
		{name: "the control at version 51", markdown: planAtVersion51, quote: "(the gate on v46).", count: 3},
		{name: "two paragraphs and the list's join", markdown: twoInside, quote: "v46). The", count: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyOperations(tree, []model.EditOp{{Op: "replace", Find: test.quote, With: "x"}})
			var ambiguous *ErrAnchorAmbiguous
			if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != test.count {
				t.Fatalf("replace of %q = %v, want TARGET_AMBIGUOUS naming %d places", test.quote, err, test.count)
			}
			for _, candidate := range ambiguous.Candidates {
				if pmdoc.TargetSpansBlocks(tree, candidate.Range) {
					t.Fatalf("candidate %v spans blocks: %q", candidate.Range, candidate.Context)
				}
			}
			if want := fmt.Sprintf("operation 0: quote %q matches %d places; ", test.quote, test.count); !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("refusal = %q, want it to start %q", err, want)
			}
		})
	}
	tree, err := parseInput(planAtVersion50)
	if err != nil {
		t.Fatal(err)
	}
	first, second := 0, 1
	out, err := applyAlone(tree, model.EditOp{Op: "replace", Find: "v46). The", With: "v46). Then the", Occurrence: &first})
	if err != nil {
		t.Fatalf("occurrence 0 at version 50 = %v, want the paragraph's match", err)
	}
	if markdown, err := renderTree(out); err != nil || !strings.Contains(markdown, "(the gate on v46). Then the workflows") {
		t.Fatalf("after occurrence 0 = %q, %v; want the paragraph's match replaced", markdown, err)
	}
	_, err = applyAlone(tree, model.EditOp{Op: "replace", Find: "v46). The", With: "x", Occurrence: &second})
	var missing *ErrQuoteNotFound
	if !errors.As(err, &missing) || missing.Matches != 1 || !strings.Contains(err.Error(), "matches 1 place,") {
		t.Fatalf("occurrence 1 at version 50 = %v, want the one match inside a block named, past which it is out of range", err)
	}
}
