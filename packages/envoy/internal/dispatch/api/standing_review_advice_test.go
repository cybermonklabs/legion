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

// standingReviewQuote is the passage of restoreSpec the anchored cases quote. No live edit they
// make touches it, so every question they open finds it. Its words appear in no question.
const standingReviewQuote = "Announce the dates."

// askAnchoredStandingReview opens standingReviewQuestion on the issue as askStandingReview does,
// anchored on standingReviewQuote in its specification, and answers the version the anchor is
// pinned to and the review the write's advice carried, nil when it carried none.
func askAnchoredStandingReview(t *testing.T, handler http.Handler, issueKey string) (int, *standingReview) {
	t.Helper()
	asked := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": standingReviewQuestion, "actor": adviceSessionActor("session-asking"),
		"anchor": map[string]any{"artifact": "spec", "quote": standingReviewQuote},
	})
	advice := adviceFromResponse(t, asked.Code, asked.Body.String())
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, asked)
	return ask.Anchor.Version, advice.StandingReview
}

// approveThenEditLive writes restoreSpec into a new issue's spec, has alice approve version 1 and
// then changes the spec's paragraph in the live document, storing no version: the documents
// settle nothing for an hour, and the change goes into the live document as an upload's does
// (replaceDocumentText), so version 1 stays the newest the table holds until a write snapshots
// the document. It answers the handler, the issue's key, its spec's id and a writer that puts
// markdown into the live document the same way.
func approveThenEditLive(t *testing.T, project string) (http.Handler, string, string, func(markdown string)) {
	t.Helper()
	handler, database, documents := settlingCarryDocuments(t, pastTheRun)
	issue := createInteractionIssue(t, handler, project, "Edited live", restoreSpec)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", standingReviewReason)
	editLive := func(markdown string) {
		t.Helper()
		if _, err := replaceDocumentText(database, documents, issue.PrimaryArtifactID, markdown,
			model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
			t.Fatalf("write the live document: %v", err)
		}
	}
	editLive(strings.Replace(restoreSpec, "behind a flag.", "behind a flag and a canary.", 1))
	return handler, issue.Key, issue.PrimaryArtifactID, editLive
}

// A person's review of the version the specification is at now is named, whether they approved or
// asked for changes: the reviewer, the time the review was recorded, the state, the version and
// the reason.
func TestAskAdviceNamesThePersonsReviewOfTheCurrentVersion(t *testing.T) {
	t.Parallel()
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
			if review.CarriedFromVersion != 0 {
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
	t.Parallel()
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
	t.Parallel()
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
// 1, and does it again, which carries that carry to version 5. The newest carry's reason names
// version 1, the version alice approved, and then version 3, whose text version 5 holds
// (docs' restoreCarriedReason), and the version it names first is what the advice must carry.
func TestAskAdviceNamesACarriedReviewAndTheVersionItsReasonGives(t *testing.T) {
	t.Parallel()
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "STANDCARRY", "Carried twice", restoreSpec)
	approval := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", standingReviewReason)

	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	agentEdit(t, handler, issue.PrimaryArtifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 1, restoreSentence(1), nil)
	agentEdit(t, handler, issue.PrimaryArtifactID, 4, replaceOp("behind a flag.", "behind a flag and a canary."))
	agentEdit(t, handler, issue.PrimaryArtifactID, 5, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 5, 3, restoreCarriedSentence(1, 3), nil)

	review := askStandingReview(t, handler, issue.Key)
	if review == nil {
		t.Fatal("the question's advice named no review; want the approval carried to version 5")
	}
	if review.CarriedFromVersion != 1 {
		t.Fatalf("standing review = %#v, want a carry naming version 1, the version the newest carry's reason gives first", review)
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

// A question anchored on the specification pins its anchor to a stored version, and where the
// live text has changed since the newest one, the anchor's snapshot writes the next version in
// the question's own transaction (resolveAnchor). Alice approved version 1 and the spec was then
// edited live, so the question's own write leaves the document at version 2, which nobody has
// reviewed: the advice names nothing. The table still holds version 1 as the newest when the
// advice is read, in the question's read phase, so the rule takes the version off the question's
// ledger (docs.Ledger.PendingVersionNumber); read off the table, it would name alice's approval of
// the text this very write moved past.
func TestAskAdviceNamesNoReviewOfTheVersionAnAnchorsSnapshotSupersedes(t *testing.T) {
	t.Parallel()
	handler, issueKey, _, _ := approveThenEditLive(t, "STANDSNAP")

	version, review := askAnchoredStandingReview(t, handler, issueKey)
	if version != 2 {
		t.Fatalf("the anchor is pinned to version %d, want 2, the version its snapshot of the live edit writes", version)
	}
	if review != nil {
		t.Fatalf("standing review = %#v, want none: alice reviewed version 1, and the question's own write leaves version 2", review)
	}
}

// The live text put back to the text alice approved, the next anchored question's snapshot
// writes version 3, which holds version 1's text byte for byte, so the restore carries her
// approval to it in the same transaction (docs.carryApproval). The advice names that carry:
// alice's approval, pinned to version 3 and naming version 1. The advice reads the stored reviews
// in the read phase and takes the carry the write phase kept once Commit has returned
// (docs.Ledger.WrittenReview), and the carry's time is its row's.
//
// The same restore over a record that moved since the approval is no restore of what alice
// approved, whatever its text. A requirement uploaded beside the spec, in the issue's
// Verification document, is a node of the spec's record - the spec query takes requirements
// wherever the issue's tree holds them - and writes no version of the spec, so the record the
// carry to version 3 pinned is no longer the spec's. The live text changed and put back again,
// the next anchored question's snapshot writes version 5, version 3's text byte for byte: the
// write phase pins its carry to that digest behind a savepoint, finds the version's record
// another digest, and drops the carry. The advice names no review: the newest one is the carry to
// version 3, and the spec is at version 5. Read in the read phase, it would name the carry to
// version 5 that the write phase dropped.
func TestAskAdviceNamesTheApprovalAnAnchorsSnapshotCarries(t *testing.T) {
	t.Parallel()
	handler, issueKey, artifactID, editLive := approveThenEditLive(t, "STANDBACK")
	if version, _ := askAnchoredStandingReview(t, handler, issueKey); version != 2 {
		t.Fatalf("the first anchor is pinned to version %d, want 2, the version its snapshot of the live edit writes", version)
	}
	editLive(restoreSpec)

	version, review := askAnchoredStandingReview(t, handler, issueKey)
	if version != 3 {
		t.Fatalf("the anchor is pinned to version %d, want 3, the version its snapshot of the restored text writes", version)
	}
	if review == nil {
		t.Fatal("the question's advice named no review; want alice's approval, carried to version 3")
	}
	if review.CarriedFromVersion != 1 {
		t.Fatalf("standing review = %#v, want a carry naming version 1, the version alice approved", review)
	}
	if review.By != "alice" || review.State != "approved" || review.Version != 3 {
		t.Fatalf("standing review = %#v, want alice's approval pinned to version 3", review)
	}
	if review.Reason != "" {
		t.Fatalf("standing review reason = %q, want none on a carry: that reason is Dispatch's own sentence", review.Reason)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list reviews: status=%d body=%s", listed.Code, listed.Body.String())
	}
	stored := decodeBody[[]model.ArtifactReview](t, listed)
	if len(stored) == 0 || stored[0].Version != 3 || stored[0].State != "approved" {
		t.Fatalf("reviews = %#v, want the carry to version 3 newest", stored)
	}
	if review.At.IsZero() || !review.At.Equal(stored[0].CreatedAt) {
		t.Fatalf("standing review at = %s, want the stored carry's own time %s", review.At, stored[0].CreatedAt)
	}

	uploadDocument(t, handler, "/api/v1/issues/"+issueKey, "verification.md", requirementMarkdown("r1", "Each handshake is logged."))
	editLive(strings.Replace(restoreSpec, "behind a flag.", "behind a flag and a canary.", 1))
	if version, _ := askAnchoredStandingReview(t, handler, issueKey); version != 4 {
		t.Fatalf("the third anchor is pinned to version %d, want 4, the version its snapshot of the live edit writes", version)
	}
	editLive(restoreSpec)
	version, review = askAnchoredStandingReview(t, handler, issueKey)
	if version != 5 {
		t.Fatalf("the fourth anchor is pinned to version %d, want 5, the version its snapshot of the restored text writes", version)
	}
	if review != nil {
		t.Fatalf("standing review = %#v, want none: version 5 restores the text of version 3 over another record, so no carry reaches it", review)
	}
	listed = dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list reviews: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if stored = decodeBody[[]model.ArtifactReview](t, listed); len(stored) != 2 || stored[0].Version != 3 {
		t.Fatalf("reviews = %#v, want alice's approval and its carry to version 3 alone", stored)
	}
}

// The cases that name no review. Each writes a question on an issue and reads the advice back,
// and each must carry none.
func TestAskAdviceNamesNoStandingReview(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
