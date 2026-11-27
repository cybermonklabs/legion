package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The record carry where the version's own write passes over it (R14): a version whose record
// digest is the approved one carries the approval at the first moment that digest is known. A
// write's rule decides in its read phase and passes over a version whose record it cannot vouch
// for there (docs' recordUnmoved); the fill of its digest after the COMMIT, which reads the
// record at the version's cut, carries it then (docs' carryUnmovedEarlier). INFRA-1098's
// architect decided it on 2026-11-26 (dispatch://INFRA-1253/comment/dfd12b58-03ff-4f84-aa01-3020f8e7651b):
// Dispatch answering "approved v3, latest v4" with no carried review at v4 is a gap in the carry,
// which the Legion daemon's design gate reads as no approval of the latest version.

// standQueryMark marks the queries of issueKey owed as a write whose drain did not run leaves
// them (a drain that failed, or a caller that held a pooled connection, docs' drainOwed): the
// mark stands until the block-index tick drains it, and every version written meanwhile is one
// whose write's rule passes over its record.
func standQueryMark(t *testing.T, database *store.Store, issueKey string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issue_query_marks (issue_key, greatest_event, greatest_seq)
		values ($1, null, (select coalesce(max(seq), 0) from graph_tuples))`, issueKey); err != nil {
		t.Fatalf("mark the queries of %s owed: %v", issueKey, err)
	}
}

// requireApprovedAtLatest fails unless GET /api/v1/artifacts/{id} answers artifactID approved at
// number, its latest version: the read the Legion daemon's design gate makes
// (Approval.ApprovedLatest).
func requireApprovedAtLatest(t *testing.T, handler http.Handler, artifactID string, number int) {
	t.Helper()
	read := readApproval(t, handler, artifactID)
	if read.Approval == nil || read.Approval.State != "approved" || read.Approval.Version == nil || *read.Approval.Version != number ||
		read.Approval.LatestVersion != number {
		t.Errorf("the approval of %s reads %+v, want approved at version %d, its latest version", artifactID, read.Approval, number)
	}
}

// A person approves CORE-1's spec, and a write moved its queries' record without a version and
// left the mark of its drain standing. A person's edit of a paragraph through the edits route then
// writes a version whose record is the approved one: the write's rule passes over it, because a
// mark stands (recordUnmoved's P2), and the fill of its digest after the COMMIT carries alice's
// approval to it, with the record carry's reason, before the route answers. The approval read
// answers the spec approved at its latest version, and the edit's drain clears the mark.
func TestTheFillCarriesAVersionWhoseWritePassedOverItsRecord(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	issue := createDigestIssue(t, handler, database, "CORE", "A spec edited under a standing mark", recordLaterSpec, false)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	awaitDigestsFilled(t, database, spec.id)
	approved := reviewDocument(t, handler, spec.id, "alice", "approved", "").Version
	standQueryMark(t, database, issue.Key)

	editThroughTheArtifactRoute(t, handler, spec.id, []map[string]any{{"op": "replace", "find": "A paragraph after it.", "with": "A paragraph after it, edited."}})
	awaitDigestsFilled(t, database, spec.id)
	edited := newestVersionOf(t, database, spec.id)
	if edited != approved+1 {
		t.Fatalf("the edit of a paragraph wrote version %d of %s, want %d", edited, spec.refKey, approved+1)
	}
	requireApprovalCarriedByRecord(t, handler, database, spec.id, approved, edited)
	requireApprovedAtLatest(t, handler, spec.id, edited)
	requireNoQueryMark(t, database, "after the edit's drain", issue.Key)
}

// A person approves CORE-1's spec, and an upload appends 2,001 list items to it, which its write
// leaves to a later pass (R15): the write states no record of its version, so its rule passes over
// it, and its row holds no time. A browser then adds a paragraph to the spec, and the settlement
// writes the next version, whose record is the approved one, and states it in its room pass: the
// settlement's rule passes over it too, since the version before it holds no record digest
// (recordUnmoved's P1). The fill after the settlement's COMMIT reads its digest, the approved
// version's, and carries alice's approval to it with the record carry's reason, so the approval
// read answers the spec approved at its latest version. The deferred version itself, no longer the
// latest, carries nothing: the next version's write weighed the approval. The reconciliation's
// first pass of a deferred version that stays the latest carries at that pass
// (TestARecordCarryWaitsForTheRequestOrTheReopen).
func TestASettlementsVersionAfterADeferredOneCarriesTheApproval(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createDigestIssue(t, handler, database, "CORE", "A spec past the bound a browser edits", recordLaterSpec, false)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	awaitDigestsFilled(t, database, spec.id)
	approved := reviewDocument(t, handler, spec.id, "alice", "approved", "").Version

	_, text := documentTexts(t, handler, spec.id)
	uploadAs(t, handler, issue.Key, documentName(t, database, spec.id), text+"\n"+strings.Repeat("- an item\n", 2001), "alice")
	deferred := newestVersionOf(t, database, spec.id)
	if deferred != approved+1 {
		t.Fatalf("the upload past the bound wrote version %d of %s, want %d", deferred, spec.refKey, approved+1)
	}
	if row := versionFillRowOf(t, database, spec.id, deferred); row.at != nil {
		t.Fatalf("version %d of %s holds the time %s once its upload answered, want none: its write leaves it to a later pass", deferred, spec.refKey, row.at)
	}
	if reviews := listReviews(t, handler, spec.id); len(reviews) == 0 || reviews[0].Version != approved {
		t.Fatalf("the reviews of %s read %+v after the upload past the bound, want alice's approval of version %d newest: the write states no record of its version", spec.refKey, reviews, approved)
	}

	peer := connectBrowserPeer(t, handler, documents, spec.id)
	stored := storedUpdateCount(t, database, spec.id)
	peer.appendParagraph(t, "A paragraph a browser adds.")
	peer.barrier(t)
	awaitStoredUpdates(t, database, spec.id, stored+1)
	settleIssue(t, documents, issue.Key)
	awaitSettled(t, database, spec.id)
	awaitDigestsFilled(t, database, spec.id)
	settled := newestVersionOf(t, database, spec.id)
	if settled != deferred+1 {
		t.Fatalf("the settlement of the browser's paragraph left version %d of %s newest, want %d", settled, spec.refKey, deferred+1)
	}
	requireApprovalCarriedByRecord(t, handler, database, spec.id, approved, settled)
	requireApprovedAtLatest(t, handler, spec.id, settled)
}
