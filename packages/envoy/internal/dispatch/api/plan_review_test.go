package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A plan is approved by the plan review that read it, never by a person: the review route records
// that approval of one version, the document then reads as approved, a later version makes it
// stale, and no approval request for a plan ever reaches a human.
func TestPlanReviewApprovesAPlanVersionAndNoPersonIsAsked(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLAN", "Approve the plan", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	plan := uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nOne.\n")

	recorded := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 1})
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the plan review: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	if review := decodeBody[model.ArtifactReview](t, recorded); review.Kind != "plan_review" || review.State != "approved" || review.Version != 1 {
		t.Fatalf("plan review = %#v, want an approved plan_review of version 1", review)
	}
	if reviews := listReviews(t, handler, plan.Artifact.ID); len(reviews) != 1 || reviews[0].Kind != "plan_review" {
		t.Fatalf("reviews of the plan = %#v, want the one plan review", reviews)
	}
	if read := readApproval(t, handler, plan.Artifact.ID); read.Approval == nil || read.Approval.State != "approved" {
		t.Fatalf("plan approval = %#v, want approved", read.Approval)
	}

	// A request for a human's approval of a plan is refused outright.
	refused := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/approval-requests", map[string]any{"actor": sessionActor()})
	code, _ := refusalOf(t, refused)
	if refused.Code != http.StatusConflict || code != "PLAN_APPROVED_BY_REVIEW" {
		t.Fatalf("approval request on a plan: status=%d code=%s body=%s", refused.Code, code, refused.Body.String())
	}

	// A later plan version makes the review's approval stale, as a spec's approval goes stale.
	uploadDocument(t, handler, owner, "plan.md", "# Plan\n\nTwo.\n")
	if read := readApproval(t, handler, plan.Artifact.ID); read.Approval.State != "stale" || read.Approval.LatestVersion != 2 {
		t.Fatalf("plan approval after a new version = %#v, want stale at version 2", read.Approval)
	}
	if again := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 2}); again.Code != http.StatusCreated {
		t.Fatalf("record the plan review of version 2: status=%d body=%s", again.Code, again.Body.String())
	}
	if read := readApproval(t, handler, plan.Artifact.ID); read.Approval.State != "approved" {
		t.Fatalf("plan approval after the second review = %#v, want approved", read.Approval)
	}
}

// A plan version whose text is the text the plan review approved carries that approval
// (docs.carryApproval's restore), and the carry stays the plan review's: the carried review and
// the artifact.approved it emits both record kind plan_review, so the document's chip still reads
// "Approved by plan review" and the agent that restored the text is never recorded as a person
// approving a plan.
func TestARestoredPlanVersionCarriesThePlanReviewsOwnKind(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "PLAN", "Restore the approved plan", "A spec\n")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nThe service ships behind a flag.\n")
	if recorded := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
		map[string]any{"actor": sessionActor(), "version": 1}); recorded.Code != http.StatusCreated {
		t.Fatalf("record the plan review: status=%d body=%s", recorded.Code, recorded.Body.String())
	}

	// The agent edits the approved plan and undoes the edit exactly, so version 3 is version 1's
	// text byte for byte.
	agentEdit(t, handler, plan.Artifact.ID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	agentEdit(t, handler, plan.Artifact.ID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	if restored, approved := versionMarkdown(t, handler, plan.Artifact.ID, 3), versionMarkdown(t, handler, plan.Artifact.ID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, plan.Artifact.ID, 3, 1, restoreSentence(1), nil)
	reviews := listReviews(t, handler, plan.Artifact.ID)
	if len(reviews) != 2 || reviews[0].Version != 3 || reviews[0].Kind != "plan_review" {
		t.Fatalf("reviews of the plan = %#v, want the carry of version 3 recorded as a plan review", reviews)
	}

	carried := 0
	for _, event := range carryEvents(t, handler, issue.Key) {
		if event.Type != "artifact.approved" {
			continue
		}
		payload := decodePayload[struct {
			Version int    `json:"version"`
			Kind    string `json:"kind"`
		}](t, event)
		if payload.Version != 3 {
			continue
		}
		carried++
		if payload.Kind != "plan_review" {
			t.Fatalf("the restored artifact.approved = %s, want kind plan_review", event.Payload)
		}
	}
	if carried != 1 {
		t.Fatalf("artifact.approved events for version 3 = %d, want the one the carry appended", carried)
	}
}

// Only a plan document is approved by a plan review: the issue's spec, and every other document,
// is refused.
func TestPlanReviewRefusesEveryDocumentThatIsNotAPlan(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLAN", "Not a plan", "A spec\n")
	notes := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "# Notes\n\nOne.\n")

	for _, artifactID := range []string{issue.PrimaryArtifactID, notes.Artifact.ID} {
		refused := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+artifactID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 1})
		code, _ := refusalOf(t, refused)
		if refused.Code != http.StatusConflict || code != "NOT_PLAN_DOCUMENT" {
			t.Fatalf("plan review of %s: status=%d code=%s body=%s", artifactID, refused.Code, code, refused.Body.String())
		}
		if reviews := listReviews(t, handler, artifactID); len(reviews) != 0 {
			t.Fatalf("reviews of %s after the refusal = %#v, want none", artifactID, reviews)
		}
	}
}

// A version the plan does not have is refused, so no approval ever names one nobody reviewed.
func TestPlanReviewRefusesAVersionThePlanDoesNotHave(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLAN", "One version", "A spec\n")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nOne.\n")

	for _, version := range []int{0, 2} {
		refused := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": version})
		code, _ := refusalOf(t, refused)
		if refused.Code != http.StatusBadRequest || code != "INVALID_REVIEW" {
			t.Fatalf("plan review of version %d: status=%d code=%s body=%s", version, refused.Code, code, refused.Body.String())
		}
	}
}

// A plan row a rolling deploy left with no role is still the issue's plan: its slug says so, as
// it does for every other reader of a document's role (documentPlays). The plan review takes it,
// and an approval request for a person is still refused on it.
func TestPlanReviewTakesAPlanRowThatCarriesNoRole(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLANR", "Role-less plan", "A spec\n")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nOne.\n")
	if _, err := database.Pool.Exec(context.Background(),
		`update artifacts set role = null where id = $1::uuid`, plan.Artifact.ID); err != nil {
		t.Fatalf("clear the plan's role: %v", err)
	}

	recorded := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 1})
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record the plan review of a role-less plan: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	refused := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/approval-requests", map[string]any{"actor": sessionActor()})
	code, _ := refusalOf(t, refused)
	if refused.Code != http.StatusConflict || code != "PLAN_APPROVED_BY_REVIEW" {
		t.Fatalf("approval request on a role-less plan: status=%d code=%s body=%s", refused.Code, code, refused.Body.String())
	}
}

// A role belongs to an issue's documents alone: documentRole gives a project's document none, and
// the slug fallback of documentPlays exists for an issue document whose role column a rolling
// deploy left null. So a project document named plan.md is a plain document, as the architect
// ruled (dispatch://INFRA-3327/comment/6a7a54ba-a1c3-4de8-a7b7-504a422f48cf): a person's
// approval of it is requested as any other project document's is, and no plan review takes it.
// The project's notes.md is the control: its request opens an ask the same way.
func TestAProjectDocumentNamedPlanIsAPlainDocument(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createInteractionIssue(t, handler, "PROJPLAN", "A project with documents", "A spec\n")
	type requested struct {
		Ask *struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	for _, name := range []string{"notes.md", "plan.md"} {
		document := uploadDocument(t, handler, "/api/v1/projects/PROJPLAN", name, "# "+name+"\n\nOne.\n")
		if document.Artifact.IssueKey != nil {
			t.Fatalf("%s = %#v, want a project document under no issue", name, document.Artifact)
		}
		response := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+document.Artifact.ID+"/approval-requests", map[string]any{"actor": sessionActor()})
		if response.Code != http.StatusCreated {
			t.Fatalf("approval request on the project's %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		if opened := decodeBody[requested](t, response); opened.Ask == nil || opened.Ask.ID == "" {
			t.Fatalf("approval request on the project's %s opened no ask: body=%s", name, response.Body.String())
		}
		if name != "plan.md" {
			continue
		}
		reviewed := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+document.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 1})
		code, _ := refusalOf(t, reviewed)
		if reviewed.Code != http.StatusConflict || code != "NOT_PLAN_DOCUMENT" {
			t.Fatalf("plan review of the project's plan.md: status=%d code=%s body=%s", reviewed.Code, code, reviewed.Body.String())
		}
	}
}

// A closed issue takes no approval of any kind: its plan review is refused 409 ISSUE_CLOSED, as
// every other write a closed issue refuses is. So an approval request on a closed issue's plan
// answers ISSUE_CLOSED, the real blocker, and not PLAN_APPROVED_BY_REVIEW, whose advice to record
// the approval with dispatch plan-approve would only be refused in its turn.
func TestAnApprovalRequestOnAClosedIssuesPlanNamesTheClose(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "PLANC", "Closed with its plan", "A spec\n")
	plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nOne.\n")
	if reviewed := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews", map[string]any{"actor": sessionActor(), "version": 1}); reviewed.Code != http.StatusCreated {
		t.Fatalf("record the plan review while the issue is open: status=%d body=%s", reviewed.Code, reviewed.Body.String())
	}
	closeIssue(t, handler, issue.Key)

	// The approval request, and the plan review PLAN_APPROVED_BY_REVIEW would send the caller to,
	// are both refused for the close.
	for _, refusedWrite := range []struct {
		route string
		body  map[string]any
	}{
		{route: "approval-requests", body: map[string]any{"actor": sessionActor()}},
		{route: "plan-reviews", body: map[string]any{"actor": sessionActor(), "version": 1}},
	} {
		refused := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+plan.Artifact.ID+"/"+refusedWrite.route, refusedWrite.body)
		code, _ := refusalOf(t, refused)
		if refused.Code != http.StatusConflict || code != "ISSUE_CLOSED" {
			t.Fatalf("%s on the closed issue's plan: status=%d code=%s body=%s, want 409 ISSUE_CLOSED",
				refusedWrite.route, refused.Code, code, refused.Body.String())
		}
	}
}
