package api

import (
	"context"
	"fmt"
	"testing"
	"time"
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

// The carry judges a version before the transaction's write phase stores it. Reads outside that
// transaction must still report it missing, and a stored version must keep its own text.
func TestSpecFormMarkdownReadsTheVersionItsTransactionQueued(t *testing.T) {
	tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1"})
	const stored = "# Spec\n\nThe stored text.\n"
	const pending = "# Spec\n\nThe queued text.\n"
	document := createFormIssue(t, tree.handler, tree.root, "A queued version", stored)
	artifact, err := artifactMetadata(t.Context(), tree.database.Pool, document.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := tree.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	ctx, ledger := tree.deps.Docs.Join(t.Context(), tx)
	defer ledger.Discard()
	version, err := ledger.QueueVersion(ctx, artifact.ID, pending, nil, nil)
	if err != nil || version.Number != 2 {
		t.Fatalf("queue version = %#v, %v; want version 2", version, err)
	}
	otherTx, err := tree.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer otherTx.Rollback(t.Context())
	for _, test := range []struct {
		name    string
		ctx     context.Context
		q       queryer
		version int
		want    string
		found   bool
	}{
		{"stored version", ctx, tx, 1, stored, true},
		{"no queued context", t.Context(), tx, 2, "", false},
		{"another transaction", ctx, otherTx, 2, "", false},
		{"another reader", ctx, tree.database.Pool, 2, "", false},
		{"another version", ctx, tx, 3, "", false},
		{"queued version", ctx, tx, 2, pending, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := specFormMarkdown(test.ctx, test.q, artifact, test.version)
			if (err == nil) != test.found || got != test.want {
				t.Fatalf("read version %d = %q, %v; want %q, found %t", test.version, got, err, test.want, test.found)
			}
		})
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := specFormMarkdown(ctx, tree.database.Pool, artifact, 2); err != nil || got != pending {
		t.Fatalf("committed version = %q, %v; want %q", got, err, pending)
	}
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
// form, and it stays unapproved. The control: the same steps with a statement of two sentences
// carry the approval, as they always did.
func TestTheRestoreCarryApprovesNoVersionThatBreaksTheForm(t *testing.T) {
	for _, test := range []struct {
		statement string
		breaks    bool
	}{
		{carryThreeSentences, true},
		{carryTwoSentences, false},
	} {
		tree := newSpecFormTree(t, testServerOptions{specRoots: "ACME-1", settle: 20 * time.Millisecond})
		document := createFormIssue(t, tree.handler, tree.outside, "Keep each comment", carrySpec(test.statement))
		reviewDocument(t, tree.handler, document.PrimaryArtifactID, "alice", "approved", "")
		reparentFormIssue(t, tree.handler, document.Key, tree.root)
		agentEdit(t, tree.handler, document.PrimaryArtifactID, 2, replaceOp("Dispatch keeps", "Dispatch stores"))
		agentEdit(t, tree.handler, document.PrimaryArtifactID, 3, replaceOp("Dispatch stores", "Dispatch keeps"))

		findings := specFormReadAtVersion(t, tree, document.PrimaryArtifactID, 3).Findings
		if broken := len(findings) > 0; broken != test.breaks {
			t.Fatalf("%q: the read of version 3 has findings %+v; want a break: %t", test.statement, findings, test.breaks)
		}
		if !test.breaks {
			requireReviewCarriedWith(t, tree.handler, document.PrimaryArtifactID, 3, 1, restoreSentence(1), nil)
			continue
		}
		if got := approvedVersions(t, tree, document.PrimaryArtifactID); len(got) != 1 || got[0] != 1 {
			t.Errorf("%q: the approved versions are %v; want only version 1, which the person approved outside the root", test.statement, got)
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
