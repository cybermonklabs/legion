package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/testwait"
)

// This file measures what an agent's single-document write is answered where no room of its
// document is resident on this process: none was ever loaded, or the one there was has just
// failed and been evicted, and no browser has loaded it again. A write of one document holds no
// grant of the pod's load concurrency before its own fork's (docs' transactionHolds), so the
// refusal of a room's load to a transaction holding its grant (docs' allowApply,
// errRoomLoadUnderGrant) reaches none of its operations, and each write lands: the edit is
// answered 200 and the anchored comment 201. Only a transaction's later document may be answered
// 503 for that refusal (dispatch://INFRA-179, round 10).

// singleWriteSpec is the spec each test's issue starts with: the line the edit rewrites and the
// line the comment quotes.
const singleWriteSpec = "Ship the migration.\n\nThe quoted line.\n"

// requireNoRoom fails unless no room of artifactID is resident here, so the write that follows
// meets a document no room holds.
func requireNoRoom(t *testing.T, documents *docs.Service, artifactID, why string) {
	t.Helper()
	if documents.RoomLiveForTest(artifactID) {
		t.Fatalf("a room of the document is resident %s, want none", why)
	}
}

// requireEditLands sends the edit an agent's `dispatch doc-edit` sends, replacing find with with,
// and fails unless it is answered 200.
func requireEditLands(t *testing.T, handler http.Handler, artifactID, find, with, what string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": find, "with": with}},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered status=%d body=%s, want %d", what, response.Code, response.Body.String(), http.StatusOK)
	}
}

// requireAnchoredCommentLands sends the comment the failed-room probe sends, anchored on a quote
// of the issue's spec, and fails unless it is answered 201.
func requireAnchoredCommentLands(t *testing.T, handler http.Handler, issueKey, body, what string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "The quoted line."},
		"body":   body,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("%s answered status=%d body=%s, want %d", what, response.Code, response.Body.String(), http.StatusCreated)
	}
}

// An agent's edit, and its anchored comment, of a document no room holds on this process and no
// browser has open are each answered 200 and 201: the write forks the stored document, and
// nothing under it waits for, or is refused, a room's load.
func TestASingleWriteToADocumentNoRoomHoldsLandsWithNoBrowser(t *testing.T) {
	// No settlement loads the document's room between the writes.
	handler, _, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	issue := createInteractionIssue(t, handler, "TEST", "A single write with no room", singleWriteSpec)
	if err := documents.Evict(context.Background(), issue.PrimaryArtifactID); err != nil {
		t.Fatalf("evict the spec's room: %v", err)
	}

	requireNoRoom(t, documents, issue.PrimaryArtifactID, "before the edit")
	requireEditLands(t, handler, issue.PrimaryArtifactID, "Ship", "Land", "an edit of a document no room holds")
	if err := documents.Evict(context.Background(), issue.PrimaryArtifactID); err != nil {
		t.Fatalf("evict the spec's room after the edit: %v", err)
	}
	requireNoRoom(t, documents, issue.PrimaryArtifactID, "before the anchored comment")
	requireAnchoredCommentLands(t, handler, issue.Key, "Quoted with no room.", "an anchored comment on a document no room holds")
	requireTextHolds(t, handler, issue.PrimaryArtifactID, "Land the migration.")
}

// The same writes to a document whose room has just failed - its eviction done, and no browser
// connected to load it again - are answered 200 and 201 too: the failed room's recovery leaves
// no room resident, which is the document no room holds above. The failed-room probe's recovery
// loop sends such a write (packages/dispatch/e2e/failed-room.e2e.ts).
func TestASingleWriteToADocumentWhoseRoomJustFailedLands(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	documents := deps.Docs.(*docs.Service)
	issue := createInteractionIssue(t, handler, "TEST", "A single write after a failed room", singleWriteSpec)

	failTheRoom := func(why string) {
		t.Helper()
		// A settlement loads the room where none is resident, as one owed after a write does.
		documents.ScheduleSettlement(issue.PrimaryArtifactID)
		testwait.Eventually(t, "the spec's room loaded "+why, func() bool {
			return documents.RoomLiveForTest(issue.PrimaryArtifactID)
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := documents.FailRoomForTest(ctx, issue.PrimaryArtifactID, errors.New("injected room failure")); err != nil {
			t.Fatalf("fail the spec's room %s: %v", why, err)
		}
		requireNoRoom(t, documents, issue.PrimaryArtifactID, "once the failed room's eviction ended "+why)
	}

	failTheRoom("before the edit")
	requireEditLands(t, handler, issue.PrimaryArtifactID, "Ship", "Land", "an edit of a document whose room just failed")
	failTheRoom("before the anchored comment")
	requireAnchoredCommentLands(t, handler, issue.Key, "Quoted after the failure.", "an anchored comment on a document whose room just failed")
	requireTextHolds(t, handler, issue.PrimaryArtifactID, "Land the migration.")
}

// requireTextHolds fails unless artifactID's text, as the API reads it, holds want.
func requireTextHolds(t *testing.T, handler http.Handler, artifactID, want string) {
	t.Helper()
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), want) {
		t.Fatalf("the document reads status=%d body=%s, want it to hold %q", text.Code, text.Body.String(), want)
	}
}
