package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// A start row that retries a claim in a new session meets the hold's refusal of that new session
// (supervise's mayForget). While the Envoy listener names the recorded session, which another
// claim's live process runs under, as the claim's role holder, the row answers a wait
// (wait.ErrWaiting, the one wait contract), which settle raises to Warn from waitWarnAttempts. A
// lookup the listener refuses the same way every time, a wrong bearer's 401 among them, is a fault
// no suspend fixes, and the row answers it as a failure.
//
// The control: forgetWait not matching wait.ErrWaiting fails the wait case.
func TestARetryHeldForItsRoleIsAWait(t *testing.T) {
	const taken = "ses-taken"
	// shared is a supervisor whose session taken another claim, legion-943's implementer,
	// recorded first, and whose listener answers every role lookup with lookup.
	shared := func(t *testing.T, lookup func(context.Context, string) (string, string, error)) *supervisor {
		t.Helper()
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		if _, err := sup.deps.Store.(*outboxClaimStore).HoldSessions(context.Background(), "legion",
			mustClaimToken(t, "LEGION-943", claim.RoleImplementer), supervise.RecordedHolds(taken, "")); err != nil {
			t.Fatalf("hold %s for the claim that recorded it first: %v", taken, err)
		}
		sup.deps.RoleHolder = lookup
		return sup
	}
	namesTaken := func(context.Context, string) (string, string, error) { return taken, "live", nil }
	unauthorized := func(context.Context, string) (string, string, error) {
		return "", "", fmt.Errorf("look up the holder: %w", &notify.RoleLookupRefused{Status: 401, Reason: `listener returned 401 Unauthorized: {"error":"unauthorized"}`})
	}
	t.Run("a start row", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			lookup func(context.Context, string) (string, string, error)
			wait   bool
		}{
			{"the role held under the session", namesTaken, true},
			{"a lookup the listener refuses", unauthorized, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
				putOutboxIssue(t, pool, records, issue)
				sup := shared(t, tc.lookup)
				tester := mustClaimToken(t, issue.Key, claim.RoleTester)
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: tester, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleTester,
					Generation: 1, State: supervise.StateFailed, Session: taken, SessionFile: "/tmp/tester.jsonl"}, ""); err != nil {
					t.Fatalf("create the failed tester: %v", err)
				}
				clock := time.Now()
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
					now: func() time.Time { return clock }}
				row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
					Generation: 1, Phase: phase.Testing, Task: "Test it.", FreshSession: true}, clock)
				err := runner.execute(context.Background(), row)
				var refused *supervise.RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("the start row = %v, want the hold's refusal", err)
				}
				if got := errors.Is(err, wait.ErrWaiting); got != tc.wait {
					t.Fatalf("the start row = %v, a wait %t; want a wait %t", err, got, tc.wait)
				}
				if m, _ := sup.Machine(tester); m.Claim().Session != taken {
					t.Fatalf("the tester records %q, want %s kept", m.Claim().Session, taken)
				}
			})
		}
	})
}
