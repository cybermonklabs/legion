package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// The carry of an approval (docs/approval_carry.go) writes an approval in the person's name, so
// the form gate judges the version it would approve, as it judges a person's own Approve. A
// version whose text breaks the form takes no carried approval: the person approves it, or not,
// through a judged route. Both kinds of carry go through the gate: the person's answer to a
// decision block, and a version whose text is the approved text again.

// carryThreeSentences and carryTwoSentences are a statement that breaks the form and one that
// keeps it.
const (
	carryThreeSentences = "Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows each comment."
	carryTwoSentences   = "Dispatch keeps each comment. Dispatch shows each comment."
)

// carryBlock is a decision block whose first option is a statement of The Spec in the form of an
// entry line, which the form reads wherever it stands.
func carryBlock(statement string) string {
	return fmt.Sprintf(":::ask{#which urgency=\"med\" multiple=\"false\"}\nWhich statement does this spec keep?\n\n"+
		"- **%s** `keep-comments` · under: `dispatch` · check: none\n- Keep it: The spec keeps the statement.\n:::\n", statement)
}

// carrySpec is a spec of one statement.
func carrySpec(statement string) string {
	return "# Spec\n\n:::requirement{#keep}\n" + statement + "\n:::\n"
}

// approvedVersions is each version of the document that a review approves.
func approvedVersions(t *testing.T, tree specFormTree, artifactID string) []int {
	t.Helper()
	var versions []int
	for _, review := range documentReviews(t, tree.handler, artifactID) {
		if review.State == "approved" {
			versions = append(versions, review.Version)
		}
	}
	return versions
}

// The person approves a covered spec, the agent adds a decision block whose option is a statement,
// and the person answers the block. The answer is the whole change since his approval, so the
// carry would approve the version it writes. With a statement of three sentences in it, that
// version breaks the form, and it stays unapproved. The control: the same block with a statement
// of two sentences carries the approval, as it always did.
func TestTheAnswerCarryApprovesNoVersionThatBreaksTheForm(t *testing.T) {
	for _, test := range []struct {
		statement string
		breaks    bool
	}{
		{carryThreeSentences, true},
		{carryTwoSentences, false},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		document := createFormIssue(t, tree.handler, tree.root, "Keep each message", carrySpec("Dispatch keeps each message."))
		reviewDocument(t, tree.handler, document.PrimaryArtifactID, "alice", "approved", "")
		asks := insertDecisionBlocks(t, tree.handler, document.Key, document.PrimaryArtifactID, carryBlock(test.statement), 2,
			map[string]string{"which": "Which statement does this spec keep?"})
		answerDecisionBlock(t, tree.handler, asks["which"], "alice", []string{"Keep it"}, "", nil)
		waitForArtifactVersion(t, tree.handler, document.PrimaryArtifactID, 3)

		findings := specFormReadAtVersion(t, tree, document.PrimaryArtifactID, 3).Findings
		if broken := len(findings) > 0; broken != test.breaks {
			t.Fatalf("%q: the read of version 3 has findings %+v; want a break: %t", test.statement, findings, test.breaks)
		}
		if !test.breaks {
			requireCarriedReview(t, tree.handler, document.PrimaryArtifactID, 3, 1, nil)
			continue
		}
		if got := approvedVersions(t, tree, document.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
			t.Errorf("%q: the approved versions are %v; want only version 1, which the person approved", test.statement, got)
		}
	}
}

// The person approves a spec outside every root, the issue moves under a root, and the agent
// changes the text and then puts it back. The restore carry would approve the version that holds
// the approved text again. With a statement of three sentences in it, that version breaks the
// form, and it stays unapproved: also where the changed version keeps the form, so the version
// before the carry's is no stand-in for the one it judges (spec_form.go reads the version that
// the carry queued). The control: the same steps with a statement of two sentences carry the
// approval, as they always did.
func TestTheRestoreCarryApprovesNoVersionThatBreaksTheForm(t *testing.T) {
	for _, test := range []struct {
		name          string
		statement     string
		change, back  map[string]string
		breaks        bool
		changedBreaks bool
	}{
		{"a changed word, in a text of three sentences", carryThreeSentences,
			replaceOp("Dispatch keeps", "Dispatch stores"), replaceOp("Dispatch stores", "Dispatch keeps"), true, true},
		{"a change that keeps the form, then the approved text of three sentences again", carryThreeSentences,
			replaceOp("Dispatch indexes each comment. ", ""),
			replaceOp("Dispatch keeps each comment. Dispatch shows", "Dispatch keeps each comment. Dispatch indexes each comment. Dispatch shows"),
			true, false},
		{"the control: a changed word, in a text of two sentences", carryTwoSentences,
			replaceOp("Dispatch keeps", "Dispatch stores"), replaceOp("Dispatch stores", "Dispatch keeps"), false, false},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		document := createFormIssue(t, tree.handler, tree.outside, "Keep each comment", carrySpec(test.statement))
		reviewDocument(t, tree.handler, document.PrimaryArtifactID, "alice", "approved", "")
		reparentFormIssue(t, tree.handler, document.Key, tree.root)
		agentEdit(t, tree.handler, document.PrimaryArtifactID, 2, test.change)
		if broken := len(specFormReadAtVersion(t, tree, document.PrimaryArtifactID, 2).Findings) > 0; broken != test.changedBreaks {
			t.Fatalf("%s: the read of version 2 breaks the form: %t; want %t", test.name, broken, test.changedBreaks)
		}
		agentEdit(t, tree.handler, document.PrimaryArtifactID, 3, test.back)

		findings := specFormReadAtVersion(t, tree, document.PrimaryArtifactID, 3).Findings
		if broken := len(findings) > 0; broken != test.breaks {
			t.Fatalf("%s: the read of version 3 has findings %+v; want a break: %t", test.name, findings, test.breaks)
		}
		if !test.breaks {
			requireReviewCarriedWith(t, tree.handler, document.PrimaryArtifactID, 3, 1, restoreSentence(1), nil)
			continue
		}
		if got := approvedVersions(t, tree, document.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
			t.Errorf("%s: the approved versions are %v; want only version 1, which the person approved outside the root", test.name, got)
		}
	}
}

// The person approves a spec and its Verification document outside every root, the agent changes
// the spec and requests approval of both, and the issue moves under a root, where the
// Verification document's claim breaks the form. The agent's edit then puts the approved spec
// text back with a decision block, and the person answers it: the carry would approve that
// version of the spec in her name and answer the request, which names both documents. An
// approval of that request covers both, so the carry judges both, as her own Approve does: it
// carries nothing, and the request stays open. The controls: her own Approve of the request is
// refused for the Verification document, and the same steps with a claim that keeps the form
// carry the spec's approval and answer the request, as they always did.
func TestTheCarryJudgesEachDocumentOfTheRequestItWouldAnswer(t *testing.T) {
	for _, test := range []struct {
		claim  string
		breaks bool
	}{
		{"The check passes when pull request #12 merges.", true},
		{"The check passes when the change merges.", false},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		issue := createFormIssue(t, tree.handler, tree.outside, "Keep each message", carrySpec("Dispatch keeps each message."))
		verification := uploadFormDocument(t, tree.handler, issue.Key, "verification.md", "# Verification\n\n:::check{#c-merge}\n"+test.claim+"\n:::\n")
		reviewDocument(t, tree.handler, issue.PrimaryArtifactID, "alice", "approved", "")
		reviewDocument(t, tree.handler, verification.ID, "alice", "approved", "")
		agentEdit(t, tree.handler, issue.PrimaryArtifactID, 2, replaceOp("Dispatch keeps each message.", "Dispatch stores each message."))
		request := requireFormRequestOpened(t, requestFormApproval(t, tree.handler, issue.PrimaryArtifactID, map[string]any{"include_verification": true})).Ask
		if len(request.Approval.Documents) != 2 {
			t.Fatalf("%q: the request names %+v, want the spec and the Verification document", test.claim, request.Approval.Documents)
		}
		reparentFormIssue(t, tree.handler, issue.Key, tree.root)
		if broken := len(specFormReadAtVersion(t, tree, verification.ID, 1).Findings) > 0; broken != test.breaks {
			t.Fatalf("%q: verification.md version 1 under the root breaks the form: %t, want %t", test.claim, broken, test.breaks)
		}
		if test.breaks {
			refused := answerFormApproval(t, tree.handler, request.ID, docs.ApprovalOptionApprove, "")
			if refused.Code != http.StatusConflict {
				t.Fatalf("control: the person's own Approve of the request: status=%d body=%s, want 409", refused.Code, refused.Body.String())
			}
			body := decodeBody[specFormError](t, refused)
			if body.Code != "SPEC_FORM" || len(body.Findings) != 1 || body.Findings[0].Document != "verification.md" || body.Findings[0].Version != 1 {
				t.Fatalf("control: the person's own Approve of the request is refused with %+v, want SPEC_FORM for verification.md version 1", body)
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
				t.Fatalf("%q: the request after the carry is %q, want answered", test.claim, state)
			}
			continue
		}
		if got := approvedVersions(t, tree, issue.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
			t.Errorf("%q: the approved versions of the spec are %v; want only version 1, which the person approved", test.claim, got)
		}
		if state != "open" {
			t.Errorf("%q: the carry answered the request that names verification.md version 1, which breaks the form: the request is %q, want open", test.claim, state)
		}
	}
}

// specFormReadAtVersion is GET /api/v1/artifacts/{id}/spec-form?version=N as alice reads it.
func specFormReadAtVersion(t *testing.T, tree specFormTree, artifactID string, version int) specFormRead {
	t.Helper()
	response := dispatchRequest(t, tree.handler, "GET", fmt.Sprintf("/api/v1/artifacts/%s/spec-form?version=%d", artifactID, version), nil, "alice")
	if response.Code != 200 {
		t.Fatalf("read the form of version %d of %s: status=%d body=%s", version, artifactID, response.Code, response.Body.String())
	}
	return decodeBody[specFormRead](t, response)
}
