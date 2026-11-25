package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The daemon writes the required check legion-verdict from the verdicts of the task's review steps,
// the reviewer's among them, so the check stands `success` at a head only once the reviewer has
// sent `approve` there. A reviewer that waited for every check the base requires before it sent
// `approve` would wait for that check, and the check would wait for the reviewer. So the reviewer's
// Go daemon part reads the required set from both places GitHub keeps it, the rulesets and branch
// protection, with legion-verdict filtered out of each read, and names legion-verdict as the one
// required check the reviewer does not wait for; and the composed prompt still holds the reviewer to
// every other required check: present, completed and success, and a wait while one runs. A part
// that put legion-verdict back among the checks the reviewer reads and waits for - a read without
// the filter, or the sentence gone - fails here. This shows the text is loaded, never that a
// reviewer follows it.
func TestReviewerPromptLeavesLegionVerdictOutOfTheChecksItWaitsFor(t *testing.T) {
	const (
		leftOut        = "`legion-verdict` is the one required check you do not wait for: the daemon writes it from the verdicts of the task's review steps, yours among them, so it stands `success` at a head only after you send `approve` there."
		rulesetsRead   = `gh api --paginate repos/<o>/<r>/rules/branches/<base> --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context | select(. != "legion-verdict")'`
		protectionRead = `gh api repos/<o>/<r>/branches/<base> --jq '.protection.required_status_checks.checks[].context | select(. != "legion-verdict")'`
	)
	part, err := goParts.ReadFile("go/reviewer.md")
	if err != nil {
		t.Fatalf("read go/reviewer.md: %v", err)
	}
	reviewerPart := foldWhitespace(string(part))
	for _, phrase := range []string{leftOut, rulesetsRead, protectionRead} {
		if !strings.Contains(reviewerPart, phrase) {
			t.Errorf("go/reviewer.md omits %q", phrase)
		}
	}
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	composed := foldWhitespace(composedText(t, composer, claim.RoleReviewer, false))
	for _, phrase := range []string{
		leftOut, rulesetsRead, protectionRead,
		"every required name is present, `completed` and `success`",
		"One `queued` or `in_progress`: wait.",
	} {
		if !strings.Contains(composed, phrase) {
			t.Errorf("the composed reviewer prompt omits %q", phrase)
		}
	}
}

// A side review has no phase, so its reviewer's verdict reaches the daemon only as the
// controller's relay (pull_request_verdict), and the controller relays it only when the Envoy
// delivery's sender is the reviewer it named. The requester that sent the reviewer the pull
// request is a fan or a dev-box session, so the message's reply_role names that session or no
// one, never the controller. merge-gate.md's part on side reviews therefore sends the verdict to
// the controller topic the reviewer's own Legion addressing line names, and never to reply_role.
// The part sending it to `reply_role`, or naming no topic, fails here. This shows the text is
// there, never that a reviewer follows it.
func TestTheSideReviewerSendsItsVerdictToTheControllerTopicOfItsAddressing(t *testing.T) {
	part := skillPart(t, filepath.Join("legion-worker", "references", "merge-gate.md"), "Side reviews")
	const destination = "`envoy_publish` `approve` or `changes`, with the pull request and head, to your addressing line's controller."
	if !strings.Contains(part, destination) {
		t.Errorf("merge-gate.md's part on side reviews omits %q: %q", destination, part)
	}
	if strings.Contains(part, "reply_role") {
		t.Errorf("merge-gate.md's part on side reviews still sends the verdict to `reply_role`, which names the requester and not the controller: %q", part)
	}
}

// Past review, the daemon asks the agent of each review step of the task that has passed for its
// verdict on the new head: the reviewer, and the owner once its step `acceptance:owner` has
// passed (go/architect-common.md, the `checks-red` paragraph). The architect's acceptance skill
// says the same where it says who is asked, so an owner that read only the skill does not take
// itself for no agent of a review step there. The sentence naming the reviewer alone fails here.
// This shows the text is there, never that an architect follows it.
func TestTheAcceptanceSkillNamesTheOwnerAmongTheAgentsAskedPastReview(t *testing.T) {
	part := skillPart(t, filepath.Join("legion-architect", "references", "acceptance.md"), "A conflicting head")
	const asked = "the daemon asks the reviewer to read the merge and send `approve` or `changes` on the new head with `review_verdict`, as it asks you once your step `acceptance:owner` has passed."
	if !strings.Contains(part, asked) {
		t.Errorf("acceptance.md's part on a conflicting head omits %q: %q", asked, part)
	}
	if retired := "has the daemon ask the reviewer for its verdict on the new head"; strings.Contains(part, retired) {
		t.Errorf("acceptance.md's part on a conflicting head still names the reviewer alone as asked past review: %q", retired)
	}
}

// skillPart is the part of a skill file under skills/ that the `## heading` line opens, up to the
// next `## ` heading, with its white space folded.
func skillPart(t *testing.T, file, heading string) string {
	t.Helper()
	skillPath := filepath.Join("..", "..", "..", "..", "skills", file)
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	_, after, found := strings.Cut(string(body), "\n## "+heading+"\n")
	if !found {
		t.Fatalf("%s has no part `## %s`", skillPath, heading)
	}
	part, _, _ := strings.Cut(after, "\n## ")
	return foldWhitespace(part)
}
