package api

import (
	"net/http"
	"testing"
)

// A question opened through /api/v2 on an issue whose specification carries a person's review of
// its newest version names that review on the write's advice (standing_review), as the /api/v1
// create does: both open the ask through openAsk.
func TestV2AskAdviceNamesThePersonsStandingReview(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STANDV2")
	spec := "# Transport\n\nThe design, as read.\n"
	issue := createAdviceIssue(t, handler, "STANDV2", "Reviewed spec", &spec)
	written := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", standingReviewReason)

	asked := sessionRequest(t, handler, http.MethodPost, "/api/v2/issues/"+issue.Key+"/asks", map[string]any{
		"question": standingReviewQuestion, "actor": adviceSessionActor("session-asking"),
	})
	review := adviceFromResponse(t, asked.Code, asked.Body.String()).StandingReview
	if review == nil {
		t.Fatalf("the v2 question's advice named no review; want alice's of version 1: %s", asked.Body.String())
	}
	if review.By != "alice" || review.State != "approved" || review.Version != 1 || review.Reason != standingReviewReason ||
		!review.At.Equal(written.CreatedAt) {
		t.Fatalf("the v2 question's standing review = %#v, want alice's approval of version 1 with its reason", review)
	}
}
