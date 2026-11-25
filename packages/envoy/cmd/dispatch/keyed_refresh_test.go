package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAKeyedApproveIsNotCommittedByTheRefreshItStarts: a keyed approve whose approver's
// confirmation lapses while the client holds its body back reads the approver's standing again
// once the body arrives (approverStanding), and that read refreshes the approver with Google
// inside the handler, where the request's commit step is live (store.WithBeforeCommit). The
// refresh's confirmation commits in a transaction of the shared pool; it is the person's write,
// not the approve's, so it marks the approve's Idempotency-Key nothing. The broker is down for
// that first approve (503), so Dispatch made no approve, and a repeat under the same key runs the
// approve again: the broker approves once.
//
// Control: run the refresh on context.WithoutCancel(ctx) in place of detached(ctx)
// (identity.Membership.flight). The confirmation's transaction marks the approve's key committed,
// and the repeat is 409 KEY_COMMITTED: the approve is never made.
func TestAKeyedApproveIsNotCommittedByTheRefreshItStarts(t *testing.T) {
	const key = "approve-while-the-confirmation-lapses"
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	approve := s.approver(t, handler, broker, "personal token")
	confirmedAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := s.database.Pool.QueryRow(ctx, `select confirmed_at from people where email = $1`, reassignedEmail).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	keyCommitted := func() bool {
		t.Helper()
		var committed bool
		if err := s.database.Pool.QueryRow(ctx, `select coalesce(bool_or(committed_at is not null), false) from request_keys where idempotency_key = $1`, key).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		return committed
	}
	approves := func() []string {
		var relayed []string
		for _, call := range broker.calls() {
			if strings.HasPrefix(call, http.MethodPost+" /v1/credential-requests/rec1/approve ") {
				relayed = append(relayed, call)
			}
		}
		return relayed
	}

	broker.mu.Lock()
	broker.unavailableApproves = 1
	broker.mu.Unlock()
	body := newHeldBody(approveBody)
	first := approve(approvePath, body)
	first.Header.Set("Idempotency-Key", key)
	var lapsedAt time.Time
	recorder := serveHeld(t, handler, first, body, func() {
		// The caller resolved on a fresh confirmation; it lapses while the body is held.
		if _, err := s.database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, reassignedEmail); err != nil {
			t.Fatal(err)
		}
		lapsedAt = confirmedAt()
	})
	if recorder.Code < http.StatusInternalServerError {
		t.Fatalf("the first approve, the broker down, answered %d %s; want a 5xx", recorder.Code, recorder.Body.String())
	}
	if refreshed := confirmedAt(); !refreshed.After(lapsedAt) {
		t.Fatalf("the approver's confirmation is %s after the first approve, as it lapsed at %s; want it refreshed inside the approve", refreshed, lapsedAt)
	}
	if relayed := approves(); len(relayed) != 1 {
		t.Fatalf("the first approve relayed %d approves to the broker; want 1, answered 503:\n%s", len(relayed), strings.Join(relayed, "\n"))
	}
	if keyCommitted() {
		t.Errorf("the approve's key %q is marked committed after the broker refused its only approve: the refresh's confirmation marked it", key)
	}

	repeat := approve(approvePath, strings.NewReader(approveBody))
	repeat.Header.Set("Idempotency-Key", key)
	again := httptest.NewRecorder()
	handler.ServeHTTP(again, repeat)
	if again.Code != http.StatusOK {
		t.Fatalf("the repeat of the approve under %q answered %d %s; want 200, the approve made now", key, again.Code, again.Body.String())
	}
	if relayed := approves(); len(relayed) != 2 || !slices.Equal(relayed[:1], relayed[1:]) {
		t.Fatalf("Dispatch relayed %d approves; want 2 alike, the broker refusing the first and approving the second once:\n%s", len(relayed), strings.Join(relayed, "\n"))
	}
}
