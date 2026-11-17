package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// The carry of an approval writes an approval in the person's name, so the form gate that it asks
// (carryBreaksForm) judges the terms of the version too (R50): a version that defines a term of
// the list again, or that defines one term twice, takes no carried approval. A definition is a
// term line under Terms, an option of a decision block under Terms among them. Under Terms, the
// question of a decision block is a line that is no term line (rule term-line), so the gate
// refuses such a version whatever its options say. Under another heading, an option in the form
// of a term line is no definition, and only the terms rule refuses it, when it names a defined
// term, which is what the rows under "## Wind" show.

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
// it. Under "## Wind", an option that names a term that the spec defines under Terms carries no
// approval either. The controls: under "## Wind", two options of one name that nothing defines
// are prose, and two options of two names, carry the approval as before.
func TestTheAnswerCarryApprovesNoVersionThatDefinesATermTwice(t *testing.T) {
	cold := termLine("bora", "The cold wind that falls from one mountain.")
	for _, c := range []struct {
		why, term     string
		second        string
		wind, carries bool
	}{
		{"bora twice under Terms", "levanter", termLine("bora", "The dry wind of one winter coast."), false, false},
		{"bora under Terms of the spec, and bora as an option under Wind", "bora", termLine("foehn", "The dry wind of one winter coast."), true, false},
		{"control, bora twice under Wind, which nothing defines", "levanter", termLine("bora", "The dry wind of one winter coast."), true, true},
		{"control, bora and foehn under Wind", "levanter", termLine("foehn", "The dry wind of one winter coast."), true, true},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		x := createFormIssue(t, tree.handler, tree.root, "Approved first", carriedSpec(c.term, "The east wind of one sea.", c.wind))
		approveFormDocument(t, tree.handler, x.PrimaryArtifactID)
		answerAtTheEnd(t, tree, x, "bora", cold, c.second)
		if c.carries {
			requireCarriedReview(t, tree.handler, x.PrimaryArtifactID, 3, 1, nil)
			continue
		}
		requireNoCarryOverATerm(t, tree, x, c.why)
	}
}

// The carry judges the terms of each document of the request that it would answer
// (approvalDocuments). The person approves a spec and its Verification document outside every root,
// and the Verification document defines a term under Terms. The agent changes the spec and
// requests approval of both. Under the root, B is approved with lease, and the issue then moves
// under the root, so the Verification document's lease is a second definition of a listed term.
// The agent's edit puts the approved spec text back with a decision block, and the person answers
// it: the carry would approve that version of the spec and answer the request, which names the
// Verification document too. So it carries nothing, and the request stays open. The controls:
// the person's own Approve of the request is refused for the Verification document, and the same
// steps with a new term, sirocco, carry the spec's approval and answer the request.
func TestTheCarryJudgesTheTermsOfEachDocumentOfTheRequestItWouldAnswer(t *testing.T) {
	for _, test := range []struct {
		term   string
		breaks bool
	}{
		{"lease", true},
		{"sirocco", false},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		issue := createFormIssue(t, tree.handler, tree.outside, "Keep each message", carrySpec("Dispatch keeps each message."))
		verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md",
			"# Verification\n\n## Terms\n\n- "+termLine(test.term, otherLease)+"\n")
		reviewDocument(t, tree.handler, issue.PrimaryArtifactID, "alice", "approved", "")
		reviewDocument(t, tree.handler, verification.ID, "alice", "approved", "")
		agentEdit(t, tree.handler, issue.PrimaryArtifactID, 2, replaceOp("Dispatch keeps each message.", "Dispatch stores each message."))
		request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})).Ask
		if len(request.Approval.Documents) != 2 {
			t.Fatalf("%s: the request names %+v, want the spec and the Verification document", test.term, request.Approval.Documents)
		}
		listed := createFormIssue(t, tree.handler, tree.root, "Defines lease", specWithTerms(termLine("lease", leaseDefinition)))
		approveFormDocument(t, tree.handler, listed.PrimaryArtifactID)
		reparentFormIssue(t, tree.handler, issue.Key, tree.root)
		if got, count := listedOf(t, tree.handler, listed.PrimaryArtifactID, "lease"); count != 1 || got.Issue != listed.Key {
			t.Fatalf("%s: the list holds lease %d times, as %#v; want once, from %s", test.term, count, got, listed.Key)
		}
		if test.breaks {
			refused := answerFormApproval(t, tree.handler, request.ID, docs.ApprovalOptionApprove, "")
			if refused.Code != http.StatusConflict {
				t.Fatalf("control: the person's own Approve of the request: status=%d body=%s, want 409", refused.Code, refused.Body.String())
			}
			body := decodeBody[specFormError](t, refused)
			if body.Code != "SPEC_FORM" || len(body.Findings) != 1 || body.Findings[0].Document != "verification.md" ||
				body.Findings[0].Rule != "term-second-definition" || body.Findings[0].Node != "lease" {
				t.Fatalf("control: the person's own Approve of the request is refused with %+v, want the second definition of lease in verification.md", body)
			}
		}

		agentEdit(t, tree.handler, issue.PrimaryArtifactID, 3, replaceOp("Dispatch stores each message.", "Dispatch keeps each message."),
			map[string]string{"op": "insert", "after": "end", "markdown": waveAsk})
		asks := indexedDecisionBlocks(t, tree.handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"})
		answerDecisionBlock(t, tree.handler, asks["wave"], "alice", []string{"Two"}, "", nil)
		waitForArtifactVersion(t, tree.handler, issue.PrimaryArtifactID, 4)

		state := readBlockAsk(t, tree.handler, request.ID).State
		if !test.breaks {
			requireCarriedReview(t, tree.handler, issue.PrimaryArtifactID, 4, 1, &request.ID)
			if state != "answered" {
				t.Fatalf("%s: the request after the carry is %q, want answered", test.term, state)
			}
			continue
		}
		if got := approvedVersions(t, tree, issue.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
			t.Errorf("%s: the approved versions of the spec are %v; want only version 1, which the person approved", test.term, got)
		}
		if state != "open" {
			t.Errorf("%s: the carry answered the request that names verification.md, which defines lease again: the request is %q, want open", test.term, state)
		}
	}
}
