package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
)

// A close withdraws the spec's approval request and a reopen restores it with the turn it had, so
// a person who was asked to approve the spec is asked again after the reopen, as before the close
// (restoreIssueAsksTx). The settlements the closed period and the reopen leave owed save the
// blocks the close and the reopen wrote, and nothing else: neither is a change the request's agent
// made to the text it handed the person. So after both have run, the request still waits on the
// person, the spec still reads awaiting approval from the person, and the request is still in the
// person's Inbox. The browser suite drives the same path (e2e/close-withdraws-asks.e2e.ts, whose
// "Waiting on you (3)" counts this request).
func TestAReopenedIssuesApprovalRequestStillWaitsOnThePersonOnceItsDocumentsSettle(t *testing.T) {
	f := newCloseAsksFixtureSettlingAfter(t, time.Hour)
	ctx := context.Background()
	before := readBlockAsk(t, f.handler, f.approvalAsk)
	if before.State != "open" || before.WaitingOn != "human" {
		t.Fatalf("approval request before the close = %s waiting on %q, want open waiting on human", before.State, before.WaitingOn)
	}

	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "done",
	}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	// The settlement the close's write leaves owed, on the closed issue, as the resumption, the
	// room's next load or its timer runs it.
	if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
		t.Fatalf("settle the closed issue's documents: %v", err)
	}
	awaitSettled(t, f.database, f.spec, f.copy)

	settled := readIssueDetail(t, f.handler, f.issue)
	if reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "todo", "expected_sequence": settled.LastSeq,
	}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	// The settlement the reopen's write leaves owed.
	if err := f.documents.SettleIssueDocuments(ctx, f.issue); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	awaitSettled(t, f.database, f.spec, f.copy)

	if got := readBlockAsk(t, f.handler, f.approvalAsk); got.State != "open" || got.WaitingOn != "human" {
		t.Fatalf("approval request after the reopen's settlement = %s waiting on %q, want open waiting on human as before the close",
			got.State, got.WaitingOn)
	}
	if approval := readApproval(t, f.handler, f.spec).Approval; approval == nil || approval.State != "awaiting" || approval.WaitingOn != "human" {
		t.Fatalf("the spec's approval after the reopen's settlement = %#v, want awaiting the person", approval)
	}
	if inbox := inboxAskIDs(t, f.handler); !slices.Contains(inbox, f.approvalAsk) {
		t.Fatalf("inbox after the reopen's settlement = %v, want the restored approval request %s", inbox, f.approvalAsk)
	}
}
