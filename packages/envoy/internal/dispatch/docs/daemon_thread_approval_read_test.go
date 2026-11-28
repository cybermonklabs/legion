package docs_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// c-daemon-thread-replaced: "the person's prior approval reads changed since". The approval read is
// the artifact read's approval (GET /api/v1/artifacts/{id}, api's applyApprovals): an approval whose
// stand does not approve the document's latest version (docs.ApprovalStand.Approves) reads stale -
// "Approved v<n> · changed since", as the web app's chip words it - and an open approval request
// over such an approval reads awaiting, naming the approval it stands beside. The step's version
// leaves alice's approval of the version she approved where it stood, so the approval read names
// her own approval of that version, below the latest version the step wrote, and does not read
// approved.

// daemonThreadApproval is a document's approval as the approval read answers it: its state, the
// version it names, the document's latest version, who gave it, and the version it was carried
// from, nil where it names none.
type daemonThreadApproval struct {
	State         string       `json:"state"`
	LatestVersion int          `json:"latest_version"`
	Version       *int         `json:"version"`
	By            *model.Actor `json:"by"`
	CarriedFrom   *int         `json:"carried_from_version"`
}

// approvalRead is the approval of the document artifactID as GET /api/v1/artifacts/{id} answers it
// to alice, failing the test where the read answers none.
func (r *daemonThreadRig) approvalRead(t *testing.T, artifactID string) daemonThreadApproval {
	t.Helper()
	read := daemonThreadDecoded[struct {
		Approval *daemonThreadApproval `json:"approval"`
	}](t, r.request(t, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice", http.StatusOK))
	if read.Approval == nil {
		t.Fatalf("the approval read of %s answers no approval", artifactID)
	}
	return *read.Approval
}

// approvalStand is the newest review of the document artifactID and what the approval read
// compares of it (docs.ApprovalStand), read by the columns and the join the approval read reads it
// by (api's latestReviewsStatement), in docs.ReviewOrder.
func (r *daemonThreadRig) approvalStand(t *testing.T, artifactID string) (model.ArtifactReview, docs.ApprovalStand) {
	t.Helper()
	review, stand, err := docs.ScanReviewStand(r.database.Pool.QueryRow(context.Background(), `
		select `+docs.ReviewReadColumns+`, `+docs.ReviewStandColumns+`
		from artifact_reviews`+docs.ReviewStandJoin+`
		where artifact_reviews.artifact_id = $1::uuid
		order by `+model.ReviewOrder+`
		limit 1`, artifactID))
	if err != nil {
		t.Fatalf("read the newest review of %s and what the approval read compares of it: %v", artifactID, err)
	}
	return review, stand
}

// requireApprovalChangedSince fails unless the approval of the spec artifactID of the issue key,
// which alice approved at version approved, reads changed since once the step wrote version newest:
// the approval read does not read approved; it names alice's own approval of version approved,
// carried from nowhere, beside the latest version newest, and awaits the open approval request;
// and the stand the approval read reads that approval by does not approve the latest version, the
// rule by which the approval read answers stale, "changed since".
func (r *daemonThreadRig) requireApprovalChangedSince(t *testing.T, key, artifactID string, approved, newest int) {
	t.Helper()
	approval := r.approvalRead(t, artifactID)
	version, by, carried := 0, "nobody", "null"
	if approval.Version != nil {
		version = *approval.Version
	}
	if approval.By != nil {
		by = approval.By.Kind + " " + approval.By.ID
	}
	if approval.CarriedFrom != nil {
		carried = strconv.Itoa(*approval.CarriedFrom)
	}
	if approval.State == "approved" {
		t.Errorf("the approval read of the spec of %s reads approved at version %d by %s, carried from version %s, beside latest version %d, the version the step wrote: the step carried it, and no person approved that version",
			key, version, by, carried, approval.LatestVersion)
		return
	}
	if version != approved || approval.By == nil || approval.By.ID != "alice" || approval.CarriedFrom != nil ||
		approval.LatestVersion != newest || approval.State != "awaiting" {
		t.Errorf("the approval read of the spec of %s reads %s at version %d by %s, carried from version %s, beside latest version %d; want alice's own approval of version %d, changed since: carried from nowhere, beside latest version %d, which the step wrote, awaiting the open approval request",
			key, approval.State, version, by, carried, approval.LatestVersion, approved, newest)
	}
	review, stand := r.approvalStand(t, artifactID)
	if review.Version != approved || review.State != "approved" || stand.Latest != newest || stand.Approves(review.Version, stand.Latest) {
		t.Errorf("the approval read reads the newest review of the spec of %s as %s %s at version %d beside latest version %d, standing %t; want alice's approval of version %d beside latest version %d, standing false: the approval read answers it stale, changed since",
			key, review.Actor.ID, review.State, review.Version, stand.Latest, stand.Approves(review.Version, stand.Latest), approved, newest)
	}
}
