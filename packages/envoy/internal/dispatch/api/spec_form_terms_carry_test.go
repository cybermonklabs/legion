package api

import (
	"testing"
	"time"
)

// The carry of an approval writes an approval in the person's name, so the form gate that it asks
// (carryBreaksForm) judges the terms of the version too (R50): a version that defines a term of
// the list again, or that defines one term twice, takes no carried approval. A line in the form of
// a term line is a definition anywhere outside a code fence, an option of a decision block among
// them. Under Terms, the question of a decision block is a line that is no term line (rule
// term-line), so the gate refuses such a version whatever its options say. Under another heading,
// only the terms rule can refuse it, which is what the rows under "## Wind" show.

// carriedTermsBlock is a decision block whose options are the lines options.
func carriedTermsBlock(question string, options ...string) string {
	block := ":::ask{#wind urgency=\"med\" multiple=\"false\"}\n" + question + "\n\n"
	for _, option := range options {
		block += "- " + option + "\n"
	}
	return block + ":::\n"
}

// carriedSpec is a spec whose Terms define term as definition. With wind, a heading "## Wind"
// follows Terms, and a decision block that the agent adds at the end stands under it; without, the
// block stands under Terms.
func carriedSpec(term, definition string, wind bool) string {
	spec := specWithTerms(termLine(term, definition))
	if wind {
		spec += "\n## Wind\n\nThe winds that this spec names.\n"
	}
	return spec
}

// answerAtTheEnd is the agent's decision block of options at the end of the spec of issue, then
// the person's answer of the option chosen: the whole change since the person's approval, so the
// carry would approve the version that the answer writes, version 3.
func answerAtTheEnd(t *testing.T, tree specFormTree, issue formIssue, chosen string, options ...string) {
	t.Helper()
	const question = "Which wind does this spec use?"
	asks := insertDecisionBlocks(t, tree.handler, issue.Key, issue.PrimaryArtifactID, carriedTermsBlock(question, options...), 2,
		map[string]string{"wind": question})
	answerDecisionBlock(t, tree.handler, asks["wind"], "alice", []string{chosen}, "", nil)
	waitForArtifactVersion(t, tree.handler, issue.PrimaryArtifactID, 3)
}

// requireNoCarryOverATerm fails t unless the only approved version of the document is version 1
// and the read of version 3 names a second definition of a term.
func requireNoCarryOverATerm(t *testing.T, tree specFormTree, issue formIssue, why string) {
	t.Helper()
	if got := approvedVersions(t, tree, issue.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
		t.Errorf("%s: the approved versions of %s are %v; want only version 1", why, issue.Key, got)
	}
	for _, finding := range specFormReadAtVersion(t, tree, issue.PrimaryArtifactID, 3).Findings {
		if finding.Rule == "term-second-definition" {
			return
		}
	}
	t.Errorf("%s: the read of version 3 of %s names no second definition", why, issue.Key)
}

// The tester's TestTheCarryOfAnApprovalLetsNoSecondDefinitionOfAListedTermThroughFresh. X is
// approved first, and Y, approved after it, holds zephyr in the list. The agent adds a decision
// block to X whose option reads "- **zephyr**: …", and the person answers it. The version that the
// answer writes defines zephyr again, so it takes no carried approval, and the list keeps Y's
// definition: under Terms, as the tester wrote it, and under "## Wind", where the terms rule alone
// refuses it. The control: under "## Wind", the same answer with a new name, sirocco, carries the
// approval as before.
func TestTheAnswerCarryLetsNoSecondDefinitionOfAListedTermThrough(t *testing.T) {
	for _, c := range []struct {
		why, term, definition string
		wind, carries         bool
	}{
		{"zephyr under Terms", "zephyr", "The warm wind of one harbour.", false, false},
		{"zephyr under Wind", "zephyr", "The warm wind of one harbour.", true, false},
		{"control, sirocco under Wind", "sirocco", "The hot wind of one desert.", true, true},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		x := createFormIssue(t, tree.handler, tree.root, "Approved first", carriedSpec("mistral", "The cold north wind of one coast.", c.wind))
		approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
		y := createFormIssue(t, tree.handler, tree.root, "Defines zephyr", specWithTerms(termLine("zephyr", "The west wind of one valley.")))
		approveFormDocument(t, tree.handler, y.PrimaryArtifactID)
		listed := specFormTerm{Term: "zephyr", Definition: "The west wind of one valley.", Document: "spec.md", Version: 1, Issue: y.Key}
		if got, count := listedOf(t, tree.handler, y.PrimaryArtifactID, "zephyr"); count != 1 || got != listed {
			t.Fatalf("%s: control, before the block: the list holds zephyr %d times, as %#v; want %#v", c.why, count, got, listed)
		}
		answerAtTheEnd(t, tree, x, c.term, termLine(c.term, c.definition), "Other: Another wind.")

		if got, count := listedOf(t, tree.handler, y.PrimaryArtifactID, "zephyr"); count != 1 || got != listed {
			t.Errorf("%s: after the answer, the list holds zephyr %d times, as %#v; want %#v", c.why, count, got, listed)
		}
		if read := specFormReadOf(t, tree.handler, y.PrimaryArtifactID); read.FindingsTotal != 0 {
			t.Errorf("%s: the read of %s, whose zephyr the list holds, has findings %+v; want none", c.why, y.Key, read.Findings)
		}
		if c.carries {
			requireCarriedReview(t, tree.handler, x.PrimaryArtifactID, 3, 1, nil)
			continue
		}
		requireNoCarryOverATerm(t, tree, x, c.why)
	}
}

// The tester's TestTheCarryOfAnApprovalLetsNoDocumentDefineATermTwiceFresh: a decision block
// whose two options are two definitions of one new term. The version that the answer writes
// defines the term two times, so it takes no carried approval: under Terms, as the tester wrote
// it, and under "## Wind". The control: under "## Wind", two options of two names carry the
// approval as before.
func TestTheAnswerCarryApprovesNoVersionThatDefinesATermTwice(t *testing.T) {
	cold := termLine("bora", "The cold wind that falls from one mountain.")
	for _, c := range []struct {
		why           string
		second        string
		wind, carries bool
	}{
		{"bora twice under Terms", termLine("bora", "The dry wind of one winter coast."), false, false},
		{"bora twice under Wind", termLine("bora", "The dry wind of one winter coast."), true, false},
		{"control, bora and foehn under Wind", termLine("foehn", "The dry wind of one winter coast."), true, true},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		x := createFormIssue(t, tree.handler, tree.root, "Approved first", carriedSpec("levanter", "The east wind of one sea.", c.wind))
		approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
		answerAtTheEnd(t, tree, x, "bora", cold, c.second)
		if c.carries {
			requireCarriedReview(t, tree.handler, x.PrimaryArtifactID, 3, 1, nil)
			continue
		}
		requireNoCarryOverATerm(t, tree, x, c.why)
	}
}
