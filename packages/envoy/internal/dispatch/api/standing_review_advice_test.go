package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A question opened on an issue whose specification already carries a person's review names that
// review on the write's advice (writeAdvice.StandingReview). It is a fact about the document, not
// about the words of the question: every question below is worded so that it shares no word with
// the review beside it, so a review these cases find cannot have come from the related-item check
// that searches this route's own words.

// standingReviewQuestion is the question every case opens. Its words appear in no spec and in no
// reason any case writes.
const standingReviewQuestion = "Which cipher suite should the gateway negotiate?"

// standingReviewReason is the reason the reviewed specifications carry. Its words appear in no
// question.
const standingReviewReason = "Measured against last quarter's figures."

// askStandingReview opens standingReviewQuestion on the issue as an agent's session does and
// answers the review the write's advice carried, nil when it carried none.
func askStandingReview(t *testing.T, handler http.Handler, issueKey string) *standingReview {
	t.Helper()
	asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": standingReviewQuestion, "actor": adviceSessionActor("session-asking"),
	})
	return adviceFromResponse(t, asked.Code, asked.Body.String()).StandingReview
}

// A person's review of the version the specification is at now is named, whether they approved or
// asked for changes: the reviewer, the time the review was recorded, the state, the version and
// the reason.
func TestAskAdviceNamesThePersonsReviewOfTheCurrentVersion(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		state string
	}{
		{name: "an approval", key: "STANDOK", state: "approved"},
		{name: "a request for changes", key: "STANDNO", state: "changes_requested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			createAdviceProject(t, handler, test.key)
			spec := "# Transport\n\nThe design, as read.\n"
			issue := createAdviceIssue(t, handler, test.key, "Reviewed spec", &spec)
			written := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", test.state, standingReviewReason)

			review := askStandingReview(t, handler, issue.Key)
			if review == nil {
				t.Fatal("the question's advice named no review; want the one alice wrote of version 1")
			}
			if review.By != "alice" || review.State != test.state || review.Version != 1 {
				t.Fatalf("standing review = %#v, want alice's %s of version 1", review, test.state)
			}
			if review.Reason != standingReviewReason {
				t.Fatalf("standing review reason = %q, want %q", review.Reason, standingReviewReason)
			}
			if review.Carried || review.CarriedFromVersion != 0 {
				t.Fatalf("standing review = %#v, want no carry: alice reviewed this version herself", review)
			}
			if !review.At.Equal(written.CreatedAt) {
				t.Fatalf("standing review at = %s, want the review row's %s", review.At, written.CreatedAt)
			}
		})
	}
}

// An approval carries no reason of its own when the person gave none, and the advice carries the
// review all the same: the reader says so rather than quoting nothing.
func TestAskAdviceNamesAReviewThatCarriesNoReason(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STANDBARE")
	spec := "# Transport\n\nThe design, as read.\n"
	issue := createAdviceIssue(t, handler, "STANDBARE", "Approved with no reason", &spec)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	review := askStandingReview(t, handler, issue.Key)
	if review == nil || review.By != "alice" || review.State != "approved" {
		t.Fatalf("standing review = %#v, want alice's approval of version 1", review)
	}
	if review.Reason != "" {
		t.Fatalf("standing review reason = %q, want none: alice gave none", review.Reason)
	}
}

// The reason the advice carries is cut at standingReviewReasonLimit characters, so a reviewer who
// wrote pages does not put them in every question's result.
func TestAskAdviceCutsTheReviewsReasonAt500Characters(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STANDLONG")
	spec := "# Transport\n\nThe design, as read.\n"
	issue := createAdviceIssue(t, handler, "STANDLONG", "A long reason", &spec)
	long := strings.Repeat("z", 640)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", long)

	review := askStandingReview(t, handler, issue.Key)
	if review == nil || len(review.Reason) != standingReviewReasonLimit {
		t.Fatalf("standing review = %#v, want a reason of %d characters", review, standingReviewReasonLimit)
	}
}

// A review Dispatch carried forward to the version the document is at now counts, and the line
// names the version the carry's own reason gives rather than the version the row is pinned to. A
// carry can follow a carry, so this drives two in a row: alice approves version 1, the agent
// changes the spec and undoes the change, which carries her approval to version 3 naming version
// 1, and does it again, which carries that carry to version 5 naming version 3. The newest carry
// names version 3, and that is what the advice must carry.
func TestAskAdviceNamesACarriedReviewAndTheVersionItsReasonGives(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "STANDCARRY", "Carried twice", restoreSpec)
	approval := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", standingReviewReason)

	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	agentEdit(t, handler, issue.PrimaryArtifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 1, restoreSentence(1), nil)
	agentEdit(t, handler, issue.PrimaryArtifactID, 4, replaceOp("behind a flag.", "behind a flag and a canary."))
	agentEdit(t, handler, issue.PrimaryArtifactID, 5, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 5, 3, restoreSentence(3), nil)

	review := askStandingReview(t, handler, issue.Key)
	if review == nil {
		t.Fatal("the question's advice named no review; want the approval carried to version 5")
	}
	if !review.Carried || review.CarriedFromVersion != 3 {
		t.Fatalf("standing review = %#v, want a carry naming version 3, the version the newest carry's reason gives", review)
	}
	if review.By != "alice" || review.State != "approved" || review.Version != 5 {
		t.Fatalf("standing review = %#v, want alice's approval pinned to version 5", review)
	}
	// The stored reason of a carry is Dispatch's own sentence, so the advice carries none: a
	// reader printing it would quote Dispatch back to the sender as alice's words.
	if review.Reason != "" {
		t.Fatalf("standing review reason = %q, want none on a carry: that reason is Dispatch's own sentence", review.Reason)
	}
	// The time is the carry's own, recorded when Dispatch wrote it, and not the time alice
	// reviewed: her review committed in an earlier transaction, so its row is strictly older.
	if !review.At.After(approval.CreatedAt) {
		t.Fatalf("standing review at = %s, want the carry's own time, after alice's own review at %s",
			review.At, approval.CreatedAt)
	}
}

// The cases that name no review. Each writes a question on an issue and reads the advice back,
// and each must carry none.
func TestAskAdviceNamesNoStandingReview(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		setUp func(t *testing.T, handler http.Handler, issueKey, artifactID string)
		spec  *string
	}{
		{
			name: "a later version has been written since the review",
			key:  "STANDSTALE",
			setUp: func(t *testing.T, handler http.Handler, _, artifactID string) {
				reviewDocument(t, handler, artifactID, "alice", "approved", standingReviewReason)
				editAdviceDocument(t, handler, artifactID, "A sentence nobody has read.")
			},
		},
		{
			name:  "nobody has reviewed the specification",
			key:   "STANDNONE",
			setUp: func(*testing.T, http.Handler, string, string) {},
		},
		{
			name: "the newest review is a plan review of the issue's plan document",
			key:  "STANDPLAN",
			setUp: func(t *testing.T, handler http.Handler, issueKey, _ string) {
				plan := uploadDocument(t, handler, "/api/v1/issues/"+issueKey, "plan.md", "# Plan\n\nOne step.\n")
				recorded := sessionRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
					map[string]any{"actor": sessionActor(), "version": 1})
				if recorded.Code != http.StatusCreated {
					t.Fatalf("record the plan review: status=%d body=%s", recorded.Code, recorded.Body.String())
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			createAdviceProject(t, handler, test.key)
			spec := "# Transport\n\nThe design, as read.\n"
			issue := createAdviceIssue(t, handler, test.key, "No standing review", &spec)
			test.setUp(t, handler, issue.Key, issue.PrimaryArtifactID)

			if review := askStandingReview(t, handler, issue.Key); review != nil {
				t.Fatalf("standing review = %#v, want none", review)
			}
		})
	}
}

// An issue created with no specification has no primary document to read, and the question is
// written as it always was.
func TestAskAdviceNamesNoReviewWhenTheIssueHasNoSpecification(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STANDBLANK")
	issue := createAdviceIssue(t, handler, "STANDBLANK", "No spec at all", nil)

	if review := askStandingReview(t, handler, issue.Key); review != nil {
		t.Fatalf("standing review = %#v, want none: the issue has no specification", review)
	}
}

// A review an agent wrote under its person's scope is not a person's review: the actor is of kind
// user with the agent named beside it (Actor.Act), and the question names nothing.
func TestAskAdviceNamesNoReviewWrittenByAnAgentUnderAPersonsScope(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "STANDACT")
	spec := "# Transport\n\nThe design, as read.\n"
	issue := createAdviceIssue(t, handler, "STANDACT", "Approved by an agent", &spec)

	token := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
	approved := agentRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews",
		map[string]any{"state": "approved", "reason": standingReviewReason}, token)
	if approved.Code != http.StatusCreated {
		t.Fatalf("approve as alice's own token: status=%d body=%s", approved.Code, approved.Body.String())
	}
	written := decodeBody[model.ArtifactReview](t, approved)
	if written.Actor.Kind != "user" || written.Actor.Act == nil {
		t.Fatalf("the review's actor = %#v, want a user with the agent named beside them", written.Actor)
	}

	if review := askStandingReview(t, handler, issue.Key); review != nil {
		t.Fatalf("standing review = %#v, want none: an agent wrote it under alice's scope", review)
	}
}

// A question on a project document is not on an issue, so there is no specification to read and
// the advice carries no review at all.
func TestAskAdviceOnAProjectDocumentNamesNoReview(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "STANDPROJ")
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/STANDPROJ/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n\nThe design, as read.\n",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, upload)
	reviewDocument(t, handler, document.Artifact.ID, "alice", "approved", standingReviewReason)

	asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.Artifact.ID+"/asks", map[string]any{
		"question": standingReviewQuestion, "actor": adviceSessionActor("session-asking"),
	})
	if review := adviceFromResponse(t, asked.Code, asked.Body.String()).StandingReview; review != nil {
		t.Fatalf("standing review = %#v, want none: the question is on a project document", review)
	}
}

// The kind guard, over the rows the rule reads. Dispatch records a plan review on an issue's
// plan.md alone, so a plan review pinned to a specification is not a state a route can reach;
// the rule refuses it all the same, because what the line claims is that a person read the
// document, and a plan review is not a person.
func TestStandingSpecReviewRefusesAPlanReviewOfTheSpecification(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "STANDKIND")
	spec := "# Transport\n\nThe design, as read.\n"
	issue := createAdviceIssue(t, handler, "STANDKIND", "A plan review of the spec", &spec)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_reviews (artifact_id, version, state, kind, actor, event_seq)
		values ($1, 1, 'approved', $2, '{"kind":"user","id":"alice"}'::jsonb, 1)
	`, issue.PrimaryArtifactID, docs.ReviewKindPlanReview); err != nil {
		t.Fatalf("insert a plan review of the specification: %v", err)
	}

	review, err := standingSpecReview(context.Background(), database.Pool, issue.Key)
	if err != nil {
		t.Fatalf("read the standing review: %v", err)
	}
	if review != nil {
		t.Fatalf("standing review = %#v, want none: a plan review is not a person's review", review)
	}
}

// carriedApprovalVersion reads the version out of the sentence both carries write
// (carriedApprovalReasonPrefix) and reports nothing for a reason a person wrote.
func TestCarriedApprovalVersionReadsTheVersionTheCarrysReasonNames(t *testing.T) {
	for _, test := range []struct {
		name    string
		reason  string
		version int
		carried bool
	}{
		{name: "the answer carry's sentence", reason: carrySentence(2), version: 2, carried: true},
		{name: "the restore's sentence", reason: restoreSentence(11), version: 11, carried: true},
		{name: "a reviewer's own reason", reason: standingReviewReason},
		{name: "no reason at all", reason: ""},
		{name: "the prefix with no version after it", reason: carriedApprovalReasonPrefix + ": unchanged."},
		{name: "the prefix later in the reason", reason: "Not " + carrySentence(2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			version, carried := carriedApprovalVersion(test.reason)
			if carried != test.carried || version != test.version {
				t.Fatalf("carriedApprovalVersion(%q) = %d, %t; want %d, %t",
					test.reason, version, carried, test.version, test.carried)
			}
		})
	}
}
