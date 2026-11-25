package prompts

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Main's retry task of a reviewing round no review decided said "the reviewer completed its round
// on pull request #%d with no review that decides it: only an APPROVE of head %s or a
// REQUEST_CHANGES ends the round" (workflow/review.go:undecidedReason at d46ec4e0): the restarted
// reviewer's completion is recorded (a restart clears no handoff), so it owes only the review.
// INFRA-3686's R2: "a step that a text gives today goes into the reader's prompt where the prompt
// does not give it". The branch's task says stuck_round: undecided, so the paragraph of the
// reviewer's prompt that defines that word must say that the reviewer completed its round.
func TestTheReviewersPromptSaysAnUndecidedRoundWasCompleted(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := composedText(t, composer, claim.RoleReviewer, false)
	var defining []string
	for _, paragraph := range strings.Split(text, "\n") {
		if strings.Contains(paragraph, "`stuck_round: undecided`") {
			defining = append(defining, paragraph)
		}
	}
	if len(defining) == 0 {
		t.Fatalf("no paragraph of the reviewer's composed prompt names `stuck_round: undecided`")
	}
	for _, paragraph := range defining {
		if strings.Contains(paragraph, "complet") {
			return
		}
	}
	var sentence string
	for _, paragraph := range defining {
		for _, s := range strings.Split(paragraph, ". ") {
			if strings.Contains(s, "`stuck_round: undecided`") {
				sentence = s
			}
		}
	}
	t.Fatalf("the reviewer's prompt defines `stuck_round: undecided` (%q) without main's fact that the reviewer completed its round "+
		"(its completion stands, so the restarted reviewer owes only the review)", sentence)
}

// Main's retryPhase (workflow/engine.go at d46ec4e0) wrote the stuck round's reason on every retry
// that found the round stuck, the operator's among them ("the operator retried this phase's
// worker" then ": " and the reason). The operator's retry of a reviewer it stopped (restartPhase,
// "a stop is no death, so no hold may be waiting") retries a phase that is not held, and the
// branch's task carries stuck_round there too. INFRA-3686's R2: "a step that a text gives today
// goes into the reader's prompt where the prompt does not give it". The reviewer's prompt must not
// give the stuck-round fields only "When your held phase is retried".
func TestTheReviewersPromptGivesTheStuckRoundFieldsForEveryRetry(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := composedText(t, composer, claim.RoleReviewer, false)
	for _, paragraph := range strings.Split(text, "\n") {
		if !strings.Contains(paragraph, "`stuck_round` names its cause") {
			continue
		}
		if strings.Contains(paragraph, "When your held phase is retried") {
			t.Fatalf("the reviewer's prompt gives the stuck-round fields only for a retry of a held phase: %q", paragraph[:120])
		}
		return
	}
	t.Fatalf("no paragraph defines `stuck_round`")
}
