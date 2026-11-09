package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A reopen at the head the record already holds moved no head, so the arrival of that head stands:
// what the record keeps is GitHub's own time of the push that put it there, and a listed review of
// it is judged against that (internal/daemon's headArrivedAt). Writing the reopen's own clock there
// would hold every such review to a time later than the push — the approval of a head nobody has
// touched since would stop deciding its round — and a reopen at another head records that head's
// own arrival as any move does.
func TestAReopenAtTheSameHeadKeepsTheHeadsArrival(t *testing.T) {
	arrived := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	reopened := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		head string
		want time.Time
	}{
		{name: "a reopen at the recorded head", head: "head", want: arrived},
		{name: "a reopen at another head", head: "pushed", want: reopened},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: arrived, HeadArrivedAt: arrived})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := mergingEngine()
			applyRefusingNothing(t, pool, engine,
				intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", UpdatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)},
				intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.head,
					UpdatedAt: reopened, ArrivedAt: reopened, Reopened: true})

			var pr *record.PullRequest
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				var err error
				pr, err = record.NewStore().PullRequest(context.Background(), tx, "LEGION-208")
				return err
			}); err != nil {
				t.Fatalf("read the pull request record: %v", err)
			}
			if pr == nil {
				t.Fatalf("the record holds no pull request for LEGION-208")
			}
			if pr.HeadSHA != tc.head {
				t.Fatalf("the record is at %q, want %q", pr.HeadSHA, tc.head)
			}
			if !pr.HeadArrivedAt.Equal(tc.want) {
				t.Fatalf("the record's head arrival = %v, want %v", pr.HeadArrivedAt, tc.want)
			}
		})
	}
}
