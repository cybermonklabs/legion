package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

func TestAMergeForwardMessageReadsItsNumberWhenTasksShareAHead(t *testing.T) {
	for _, landed := range []bool{false, true} {
		name := "its own ask still stands"
		if landed {
			name = "its own merge already landed"
		}
		t.Run(name, func(t *testing.T) {
			asked := &record.MergeForward{Head: "sha-1", Base: "main", Phase: phase.Testing, Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
			other := *asked
			if landed {
				asked.Landed = "sha-2"
			} else {
				other.Landed = "sha-2"
			}
			publisher := &holderPublisher{}
			runner, _, pool, _ := mergeForwardRunner(t, publisher, &other)
			if err := store.Transact(t.Context(), pool, func(ctx context.Context, tx pgx.Tx) error {
				return runner.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
					Number: 87, Branch: "legion/LEGION-208.b", HeadSHA: "sha-1", HeadUpdatedAt: time.Now(), Failing: []string{},
					CheckRuns: []record.AttemptRun{}, Base: "main", MergeForward: asked})
			}); err != nil {
				t.Fatal(err)
			}
			message := record.MergeForwardMessage{Role: claim.RoleTester, Step: record.MergeForwardHold, Head: "sha-1", Base: "main",
				Phase: phase.Testing, PullRequest: 87, Why: record.ConflictCheckNotGreen}
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", message, time.Now().Add(-time.Minute)))
			drainAt(t, runner, 0, "the task's merge-forward hold")
			_, delivered := publisher.snapshot()
			want := 1
			if landed {
				want = 0
			}
			if len(delivered) != want {
				t.Fatalf("hold for pull request #87 delivered %d times, want %d; #86 shares its head but has the opposite landing state", len(delivered), want)
			}
		})
	}
}
