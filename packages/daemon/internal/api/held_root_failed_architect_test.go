package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// R20 of dispatch://INFRA-3320: "A failed role is not restarted through a status. The controller's
// set_status of todo on a root in work whose architect claim failed starts that claim again in its
// session with fresh budgets, as `legion claims retry` does ... It replaces the restart that sets
// backlog and then todo", and "A todo on a held root releases its hold, as today." The root's
// architect escalated it (a hold, record.HoldEscalated), and the architect's claim then failed. The
// root is in Legion's work, so the controller's set_status of todo is the remedy R20 gives it: it
// releases the hold, and it starts the failed architect claim again, so the released tree has an
// architect.
func TestTodoOnAHeldRootWithAFailedArchitectRestartsThatClaim(t *testing.T) {
	h, _, _ := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	failed := h.stored(architect.token)
	failed.State = supervise.StateFailed
	if err := h.store.PutClaim(context.Background(), failed); err != nil {
		t.Fatalf("fail the root's architect claim: %v", err)
	}
	root := recordedChild(t, h, statusRoot)
	if root == nil {
		t.Fatalf("no record of %s", statusRoot)
	}
	root.Phase, root.Hold = phase.Held, &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, *root)
	}); err != nil {
		t.Fatalf("hold %s: %v", statusRoot, err)
	}
	architectStarts := func() int {
		t.Helper()
		var count int
		if err := h.store.Pool().QueryRow(context.Background(),
			`select count(*) from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'architect'`,
			statusRoot).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := architectStarts()

	grant := controllerGrant(t, h)
	recorder := h.request(http.MethodPost, "/legion/v1/issues/status", IssueStatusRequest{GrantID: grant.GrantID, Issue: statusRoot, Status: "todo"}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("set_status = %d %s, want 200", recorder.Code, recorder.Body)
	}
	if got := architectStarts(); got != before+1 {
		t.Errorf("the architect starts of %s = %d, want one more than the %d before: the controller's todo starts the failed architect claim again", statusRoot, got, before)
	}
	if after := recordedChild(t, h, statusRoot); after == nil || !record.InLegionsWork(*after) || after.Phase == phase.Held {
		t.Errorf("%s after the todo = %#v, want it in Legion's work with its hold released", statusRoot, after)
	}
}
