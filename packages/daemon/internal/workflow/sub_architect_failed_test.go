package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A child's own architect's claim failing holds nothing, even at accepting, its own phase. While
// that claim no longer holds its role the child's notices go on up to the architect above it,
// which must hear why: the failure is one worker-died on the child, naming the architect, the
// child's phase and the failure the fact carries, whose reason names the child and says its
// notices come to the reader until the daemon starts the child's architect again. The owner walk
// sends it above the child (record.OwningArchitect). The controller hears nothing of it, since an
// architect inside the tree is left to act. A done child and a child that left the workflow tell
// nobody: a done child's claims were closed (retired, sessions kept), and a parked child's were
// suspended.
func TestASubArchitectsFailedClaimTellsTheArchitectAboveIt(t *testing.T) {
	const sentence = "the architect's process died with its catch-up outstanding, and its relaunches ran out"
	root := "LEGION-208"
	liveRoot := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child",
		Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"}
	// apply fails the child's architect claim, with every count against its own limit and no two
	// pairs alike, so a notice that paired a count with another's limit reads differently.
	apply := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		failed := intake.ClaimFailed{Issue: child.Key, Role: claim.RoleArchitect, Reason: sentence, Death: "the pod was evicted", Cause: supervise.CauseDied,
			Budgets: supervise.Budgets{LaunchFailures: 1, Deaths: 3, PromptFailures: 2, PromptRetires: 1},
			Limits:  supervise.Limits{LaunchFailures: 3, PromptFailures: 5, PromptRetires: 2}}
		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "sub-architect-failed", failed, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the child architect's failed claim: %v", err)
		}
	}

	t.Run("a child in the workflow", func(t *testing.T) {
		pool := migratedPool(t)
		seedIssue(t, pool, liveRoot)
		seedIssue(t, pool, child)
		before := storedIssue(t, pool, child.Key)

		apply(t, pool)
		if after := storedIssue(t, pool, child.Key); !reflect.DeepEqual(after, before) || after.Phase != phase.Testing || after.Hold != nil {
			t.Fatalf("the child after its architect's failure = %#v, want it as it was, in testing and not held", after)
		}
		// One row in the whole outbox: the worker-died, and no controller copy beside it.
		assertOutboxKinds(t, pool, []string{string(record.OutboxKindNotice)})
		died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Testing,
			Reason: "LEGION-209's own architect failed; its notices come to you until it runs again, and the daemon starts it again at LEGION-209's next phase start",
			Failure: &record.NoticeFailure{Cause: string(supervise.CauseDied), Detail: sentence,
				LaunchFailures: record.FailureBudget{Count: 1, Limit: 3},
				Deaths:         record.FailureBudget{Count: 3, Limit: 3},
				PromptFailures: record.FailureBudget{Count: 2, Limit: 5},
				PromptRetires:  record.FailureBudget{Count: 1, Limit: 2},
				Death:          "the pod was evicted"}}
		if got, want := noticeRows(t, pool), []record.OutboxPayload{died}; !sameNotices(got, want) {
			t.Fatalf("notice rows = %+v, want %+v: one worker-died naming the failure", got, want)
		}
		var on string
		if err := pool.QueryRow(t.Context(), "select issue from outbox where kind = $1", string(record.OutboxKindNotice)).Scan(&on); err != nil {
			t.Fatalf("read the worker-died's issue: %v", err)
		}
		if on != child.Key {
			t.Fatalf("the worker-died is queued on %s, want the child %s", on, child.Key)
		}
	})

	for _, tc := range []struct {
		name        string
		root, child record.Issue
	}{
		{
			name: "a done child",
			root: liveRoot,
			child: func() record.Issue {
				done := child
				done.Phase, done.Status = phase.Done, "done"
				return done
			}(),
		},
		{
			name: "a child that left the workflow",
			root: liveRoot,
			child: func() record.Issue {
				left := child
				left.Phase, left.Status = phase.Done, "backlog"
				return left
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, tc.root)
			seedIssue(t, pool, tc.child)
			before := storedIssue(t, pool, tc.child.Key)

			apply(t, pool)
			if after := storedIssue(t, pool, tc.child.Key); !reflect.DeepEqual(after, before) {
				t.Fatalf("the child after its architect's failure = %#v, want it as it was, %#v", after, before)
			}
			assertOutboxKinds(t, pool, []string{})
		})
	}
}
