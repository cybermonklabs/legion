package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A merging member of a tree whose root is done holds its merge open while the close of the
// root's linger hands it over (the hand-over's fence, workflow's mergeCanLand), even with its pull
// request closed. Dispatch refuses the hand-over for good, or a person's move supersedes it, and
// the outbox drops the fence: the merge can no longer land, so the drop settles as a fact does and
// ends the merge stopped, once. A fact after it that writes the member ends nothing more.
func TestADroppedHandOverFenceEndsTheMergeItHeldOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(t *testing.T, runner *outbox, row record.OutboxRow, payload record.StatusWrite)
	}{
		{name: "Dispatch refuses the hand-over", drop: func(t *testing.T, runner *outbox, row record.OutboxRow, _ record.StatusWrite) {
			refusal := &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no such issue"}
			if err := runner.finishRefused(context.Background(), row, refusal); err != nil {
				t.Fatalf("finish the refused hand-over: %v", err)
			}
		}},
		{name: "a person's move supersedes the hand-over", drop: func(t *testing.T, runner *outbox, row record.OutboxRow, payload record.StatusWrite) {
			if err := runner.dropHandover(context.Background(), row.Issue, payload); err != nil {
				t.Fatalf("drop the superseded hand-over's fence: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			runner := &outbox{pool: pool, records: records, log: quietLogger(), handlers: []intake.Handler{engine}}

			lingered := time.Now().Add(-time.Hour)
			parent := "LEGION-208"
			putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U",
				LastDispatchSeq: 1, LingerUntil: &lingered})
			member := record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Parent: &parent, Project: "LEGION", Title: "member", Phase: phase.Merging, Generation: 1,
				Status: "in_progress", Rank: "V", LastDispatchSeq: 1}
			putOutboxIssue(t, pool, records, member)
			merge := record.StepPlace{Task: "default", Kind: "merge", Place: 0}
			merger, err := claim.NewToken("legion", "LEGION-209", claim.RoleMerger)
			if err != nil {
				t.Fatal(err)
			}
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-209", Repo: "sjawhar/legion",
					Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}}); err != nil {
					return err
				}
				if err := records.PutHandover(ctx, tx, record.Handover{Issue: "LEGION-209", Tree: "LEGION-208", Generation: 1}); err != nil {
					return err
				}
				_, err := records.RaiseStepStarts(ctx, tx, "LEGION-209", merge, func(starts int64) record.StepRecord {
					return record.StepRecord{ID: workflow.StepID("LEGION-209", 1, merge, starts), Task: "default", Kind: "merge", Role: "merger",
						Agent: string(merger), Generation: 1}
				})
				return err
			}); err != nil {
				t.Fatalf("seed the merging member, its closed pull request, its fence and its open merge: %v", err)
			}

			payload := record.HandOverStatusWrite("todo", "in_progress", "LEGION-208", 1)
			stored, err := record.NewOutboxRow("LEGION-209", payload, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			row := leasedOutboxRow(t, pool, records, stored)
			tc.drop(t, runner, row, payload)

			ends := mergeEndsOf(t, pool, "LEGION-209")
			if len(ends) != 1 || ends[0] != "stopped" {
				t.Fatalf("the merge's ends after the fence's drop = %v, want one, stopped: its pull request is closed and nothing hands it over", ends)
			}
			if _, err := intake.ApplyFact(ctx, pool, "api", "rank", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2,
				Type: "issue.updated", Status: "in_progress", Title: "member", Parent: "LEGION-208", Rank: "W"}, engine); err != nil {
				t.Fatalf("apply a later fact of the member: %v", err)
			}
			if again := mergeEndsOf(t, pool, "LEGION-209"); len(again) != 1 {
				t.Fatalf("the merge's ends after a later fact = %v, want the one end", again)
			}
		})
	}
}

// mergeEndsOf is the result of each queued step record of issue's merge that ends it, in the
// order they were queued.
func mergeEndsOf(t *testing.T, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload->>'result' from outbox
		where kind = $1 and issue = $2 and payload->>'kind' = 'merge' and payload ? 'result' order by id`, string(record.OutboxKindStepRecord), issue)
	if err != nil {
		t.Fatalf("read the merge records of %s: %v", issue, err)
	}
	ends, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the merge records of %s: %v", issue, err)
	}
	return ends
}
