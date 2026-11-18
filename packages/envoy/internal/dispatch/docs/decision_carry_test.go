package docs

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The restore's and the conversion's carry cases (dispatch://INFRA-1150 R7 and R9; restoreCase,
// conversionCase) over the two versions, as carryNewestApproval feeds carryVerdict: the restore's
// answered asks in the spec's `## Decisions` part, in place of its pointer line to the issue's
// decisions document, under the heading the restore adds, and under a new block id, carry the
// approval, and so does an open ask block the restore moved there from the decisions document
// (D1), and such a block the approved version held elsewhere with the same words; a
// restored block whose words moved does not. The conversion's decisions in that part carry it,
// and so do the `shaped` pointers it gives an answered ask that stands in the spec already, beside
// a decision and a reading in one version (spec v42's R9); a decision outside the part does not.
//
// Each case takes out only what the step's own write added, as its mark names it
// (decisionStepCases): out of the new version the ask blocks it restored, the decisions it wrote
// and the pointers it gave an ask, and out of the approved version the pointer line it took out,
// once in a pass of the cases however many identical paragraphs the approved version holds.
// So a change an agent made since the approval, which the step's version keeps, still supersedes
// it: a decision the approved version held whose words an agent rewrote, a pointer an agent gave
// an answered ask, an answered ask block an agent wrote into the part, and a paragraph of the part
// citing a block of the decisions document, which the step keeps and a writer took out. Each
// verdict names the cases it carried by, in carryCases' order. Every row is weighed twice: as the
// decisions step's own write, and as another writer's - an agent's upload or edit making the same
// change - which carries nothing by those two cases.
func TestTheRestoreAndTheConversionCarryAnApprovalForTheStepsOwnWriteAlone(t *testing.T) {
	const prose = "# Rollout\n\n:::requirement{#r1}\nThe service ships behind a flag.\n:::\n\n"
	const pointer = "The decisions this spec rests on are in dispatch://DOC-1/artifact/decisions-md.\n"
	const cite = "We chose the flag because a rollback stays cheap (dispatch://DOC-1/artifact/decisions-md#b-h1).\n"
	ask := func(id, question, extra string) string {
		return `:::ask{#` + id + ` urgency="low" multiple="false"` + extra + "}\n" + question + "\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
	}
	askWithReading := func(id, extra string) string {
		return `::::ask{#` + id + ` urgency="low" multiple="false"` + extra + "}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n\n" +
			":::reading{#r9}\nThe agent reads it as the east first.\n:::\n::::\n"
	}
	decision := ":::decision{#d1}\nThe census runs before the view.\n:::\n"
	held := func(words string) string { return ":::decision{#d0}\n" + words + "\n:::\n" }
	const stepPointer = "dispatch://DOC-1/spec#b-r1"
	const agentPointer = "dispatch://DOC-1/spec#b-r2"
	shaped := ` shaped="` + stepPointer + `"`
	part := "## Decisions\n\n"
	restored := restoreCase.change + carriedReasonEnd
	converted := conversionCase.change + carriedReasonEnd
	parse := func(markdown string) *pmdoc.Node {
		t.Helper()
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			t.Fatalf("parse %q: %v", markdown, err)
		}
		return tree
	}
	// line is the mark's name of a pointer line the step's write took out: the paragraph's markdown
	// as the write renders it (blockMarkdown).
	line := func(paragraph string) string {
		t.Helper()
		markdown, err := blockMarkdown(parse(paragraph).Children[0])
		if err != nil {
			t.Fatalf("render %q: %v", paragraph, err)
		}
		return markdown
	}
	ids := func(blocks ...string) map[string]bool {
		set := map[string]bool{}
		for _, id := range blocks {
			set[id] = true
		}
		return set
	}
	tookPointer := []string{line(pointer)}
	for _, test := range []struct {
		name              string
		approved, version string
		decided, answered map[string]bool
		// mark is what the step's write added, as its version's mark names it, its phases aside.
		mark   decisionStepCases
		change string
	}{
		{"an answered ask returned in place of the pointer line", prose + part + pointer, prose + part + ask("q1", "Which region goes first?", ""),
			ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, restored},
		{"an answered ask returned under the heading the restore adds", prose, prose + part + ask("q1", "Which region goes first?", ""),
			ids("q1"), nil, decisionStepCases{restored: ids("q1")}, restored},
		{"an answered ask the approved version held elsewhere, returned with its words", prose + ask("q1", "Which region goes first?", "") + "\n" + part + pointer,
			prose + part + ask("q1", "Which region goes first?", ""), ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, restored},
		{"an answered ask returned under a new block id", prose + part + pointer, prose + part + ask("q-fresh", "Which region goes first?", ""),
			ids("q-fresh"), nil, decisionStepCases{restored: ids("q-fresh"), pointerLines: tookPointer}, restored},
		{"an answered ask returned with its question edited", prose + ask("q1", "Which region goes first?", "") + "\n" + part + pointer,
			prose + part + ask("q1", "Which region goes last?", ""), ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, ""},
		{"an open ask the restore moved into the Decisions part", prose + part + pointer, prose + part + ask("q1", "Which region goes first?", ""),
			map[string]bool{"q1": false}, nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, restored},
		{"a decision in place of the pointer line", prose + part + pointer, prose + part + decision, nil, nil,
			decisionStepCases{decisions: ids("d1"), pointerLines: tookPointer}, converted},
		{"a decision under the heading the conversion adds", prose, prose + part + decision, nil, nil,
			decisionStepCases{decisions: ids("d1")}, converted},
		{"a shaped pointer the conversion gives a present answered ask, beside a decision and a reading",
			prose + ask("q1", "Which region goes first?", "") + "\n" + part + pointer,
			prose + askWithReading("q1", shaped) + "\n" + part + decision,
			ids("q1"), ids("q1"), decisionStepCases{decisions: ids("d1"), shaped: map[string][]string{"q1": {stepPointer}}, pointerLines: tookPointer},
			readingCase.change + ", and " + conversionCase.change + carriedReasonEnd},
		{"a decision outside the Decisions part", prose + part + pointer, prose + decision + "\n" + part + pointer, nil, nil,
			decisionStepCases{decisions: ids("d1")}, ""},
		{"a restored ask and a decision in one version", prose + part + pointer,
			prose + part + ask("q1", "Which region goes first?", "") + "\n" + decision,
			ids("q1"), nil, decisionStepCases{restored: ids("q1"), decisions: ids("d1"), pointerLines: tookPointer},
			restoreCase.change + ", and " + conversionCase.change + carriedReasonEnd},
		{"prose changed beside a decision", prose + part + pointer, strings.Replace(prose, "a flag", "a switch", 1) + part + decision, nil, nil,
			decisionStepCases{decisions: ids("d1"), pointerLines: tookPointer}, ""},
		{"a decision the approved version held, unchanged, beside the conversion's own", prose + part + pointer + "\n" + held("The census runs first."),
			prose + part + decision + "\n" + held("The census runs first."), nil, nil,
			decisionStepCases{decisions: ids("d1"), pointerLines: tookPointer}, converted},
		{"a decision the approved version held, whose words an agent rewrote, beside the conversion's own", prose + part + pointer + "\n" + held("The census runs first."),
			prose + part + decision + "\n" + held("The census runs last."), nil, nil,
			decisionStepCases{decisions: ids("d1"), pointerLines: tookPointer}, ""},
		{"a shaped pointer an agent gave a present answered ask, beside the conversion's own",
			prose + ask("q1", "Which region goes first?", "") + "\n" + part + pointer,
			prose + ask("q1", "Which region goes first?", ` shaped="`+agentPointer+" "+stepPointer+`"`) + "\n" + part + decision,
			ids("q1"), nil, decisionStepCases{decisions: ids("d1"), shaped: map[string][]string{"q1": {stepPointer}}, pointerLines: tookPointer}, ""},
		{"an answered ask an agent wrote into the Decisions part, beside the restore's own", prose + part + pointer,
			prose + part + ask("q1", "Which region goes first?", "") + "\n" + ask("q2", "Which region goes last?", ""),
			ids("q1", "q2"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, ""},
		{"a paragraph citing a block of the decisions document, kept beside the restore's ask", prose + part + pointer + "\n" + cite,
			prose + part + ask("q1", "Which region goes first?", "") + "\n" + cite,
			ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, restored},
		{"a paragraph citing a block of the decisions document, taken out beside the restore's ask", prose + part + pointer + "\n" + cite,
			prose + part + ask("q1", "Which region goes first?", ""),
			ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, ""},
		{"two identical pointer lines the approved version held, of which the step's write took out one", prose + part + pointer + "\n" + pointer,
			prose + part + ask("q1", "Which region goes first?", "") + "\n" + pointer,
			ids("q1"), nil, decisionStepCases{restored: ids("q1"), pointerLines: tookPointer}, restored},
	} {
		approvedText, err := pmdoc.Render(parse(test.approved))
		if err != nil {
			t.Fatalf("%s: render the approved version: %v", test.name, err)
		}
		approved, version := parse(approvedText), parse(test.version)
		for _, step := range []bool{true, false} {
			input := carryInput{
				approved: approved, approvedText: approvedText, version: version, agrees: true,
				answered: test.answered, decided: test.decided,
			}
			want := test.change
			if step {
				input.steps = test.mark
				input.steps.restore, input.steps.conversion = true, true
				input.restored = restoredAskBlocks(approved, version, input.steps.restored)
			} else if want != "" {
				// Another writer's version making the same change: a route's, an agent's upload.
				want = ""
			}
			change, carried, err := carryVerdict(input)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if carried != (want != "") || change != want {
				t.Errorf("%s, the step's own write %v: carry = %v with the change %q, want carry %v with %q", test.name, step, carried, change, want != "", want)
			}
		}
	}
}
