package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A child handed over when its root closes carries its open pull request into its new
// generation. The head's CI settlement the record holds is the listener's, with the listener's
// generation, and a settlement of that head from before it, delivered late or replayed from a
// message the intake gave up on, is as stale after the carry as it was before: the record keeps
// the newer settlement's generation and snapshot, and the later status of its commit.
func TestAnEarlierSettlementOfACarriedPullRequestsHeadChangesNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child, head = "LEGION-208", "LEGION-209", "head-42"
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1}),
			records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: child, Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-209",
				HeadSHA: head, State: record.PullRequestOpen, Failing: []string{}, Cancelled: []string{}}),
			records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})
	at := func(minute int) time.Time { return time.Date(2026, 9, 23, 11, minute, 0, 0, time.UTC) }
	settlement := func(generation int64, state record.CommitStatusState, minute int) intake.PullRequestChecks {
		return intake.PullRequestChecks{Repo: "acme/widgets", Number: 42, HeadSHA: head, CheckRuns: []record.AttemptRun{{Name: "ci", ID: 5}},
			Generation: generation, Snapshot: fmt.Sprintf("snapshot-%d", generation), Failing: []string{}, Cancelled: []string{},
			Statuses: []record.CommitStatus{{Context: "deploy", State: state, CreatedAt: at(minute)}}}
	}
	settle := func(eventID string, fact intake.PullRequestChecks) record.PullRequest {
		t.Helper()
		if _, err := intake.ApplyFact(context.Background(), pool, "github", eventID, fact, engine, admission); err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		var pr *record.PullRequest
		inTx(t, pool, func(tx pgx.Tx) {
			var err error
			if pr, err = record.NewStore().MergePullRequest(context.Background(), tx, child); err != nil || pr == nil {
				t.Fatalf("read the pull request after %s: %v, %v", eventID, pr, err)
			}
		})
		return *pr
	}
	newer, earlier := settlement(40, record.CommitStatusSuccess, 2), settlement(30, record.CommitStatusFailure, 1)

	held := settle("newer", newer)
	if held.Generation != 40 || held.Snapshot != newer.Snapshot {
		t.Fatalf("the newer settlement left generation %d, snapshot %q; want 40, %q", held.Generation, held.Snapshot, newer.Snapshot)
	}
	if got := settle("earlier-before-the-carry", earlier); got.Generation != 40 || got.Snapshot != newer.Snapshot {
		t.Fatalf("before the carry the earlier settlement left generation %d, snapshot %q; want it stale: 40, %q", got.Generation, got.Snapshot, newer.Snapshot)
	}

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, engine)
	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 {
		t.Fatalf("handed-over child = tree %s, generation %d; want its own tree at generation 2", got.Tree, got.Generation)
	}

	got := settle("earlier-after-the-carry", earlier)
	if got.Generation != 40 || got.Snapshot != newer.Snapshot {
		t.Errorf("after the carry the earlier settlement left generation %d, snapshot %q; want it stale as before the carry: 40, %q",
			got.Generation, got.Snapshot, newer.Snapshot)
	}
	for _, status := range got.Statuses {
		if status.Context == "deploy" && status.State != record.CommitStatusSuccess {
			t.Errorf("after the carry the earlier settlement put deploy back to %s; want the later success", status.State)
		}
	}
}
