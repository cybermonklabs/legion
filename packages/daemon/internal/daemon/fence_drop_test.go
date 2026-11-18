package daemon

import (
	"context"
	"fmt"
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
	"github.com/sjawhar/legion/daemon/internal/store"
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

// A re-admission cancels a hand-over's fence, and the hand-over row's check drops it
// (handOverHolds). The drop matches the tree and the linger generation, not the cancel, so a fact
// that put the fence again for the same linger between the check's read and its drop would lose
// the fence it put. The read takes no fact lock, so here such a fact commits right after it; the
// drop reads the fence again under the lock every fact applies under and deletes it only while it
// is still cancelled, so the fence the fact put stands after the drop, not cancelled.
func TestAFenceAFactPutsAgainOutlivesTheDropOfTheCancelledOne(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	racing := &factPutsTheFenceAgain{Store: records, pool: pool}
	runner := &outbox{pool: pool, records: racing, log: quietLogger(), handlers: []intake.Handler{engine}}

	lingered := time.Now().Add(time.Hour)
	parent := "LEGION-208"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U",
		LastDispatchSeq: 1, LingerUntil: &lingered})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Parent: &parent, Project: "LEGION", Title: "member", Phase: phase.Testing, Generation: 1,
		Status: "testing", Rank: "V", LastDispatchSeq: 1})
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutHandover(ctx, tx, record.Handover{Issue: "LEGION-209", Tree: "LEGION-208", Generation: 1}); err != nil {
			return err
		}
		return records.CancelTreeHandovers(ctx, tx, "LEGION-208", 1)
	}); err != nil {
		t.Fatalf("seed the member's cancelled fence: %v", err)
	}

	payload := record.HandOverStatusWrite("todo", "testing", "LEGION-208", 1)
	stored, err := record.NewOutboxRow("LEGION-209", payload, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	row := leasedOutboxRow(t, pool, records, stored)
	if holds, err := runner.handOverHolds(ctx, row, payload); err != nil || holds {
		t.Fatalf("the check of the cancelled hand-over holds %t, %v; want false", holds, err)
	}
	if err := <-racing.put; err != nil {
		t.Fatalf("the fact's put of the fence: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		fence, err := records.Handover(ctx, tx, "LEGION-209")
		if err != nil {
			return err
		}
		if fence == nil || fence.Cancelled || fence.Tree != "LEGION-208" || fence.Generation != 1 {
			t.Errorf("the member's fence after the drop = %+v, want the one the fact put: LEGION-208's linger 1, not cancelled", fence)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the member's fence: %v", err)
	}
}

// factPutsTheFenceAgain is the store, but for its first read of a hand-over fence: right after
// that read it starts a fact, on a connection of its own under the lock every fact applies under,
// that puts the fence it read again, not cancelled, and goes on once that fact has committed or
// waits for the lock. put answers the fact's result.
type factPutsTheFenceAgain struct {
	record.Store
	pool *store.Pool
	put  chan error
}

func (s *factPutsTheFenceAgain) Handover(ctx context.Context, tx pgx.Tx, issue string) (*record.Handover, error) {
	read, err := s.Store.Handover(ctx, tx, issue)
	if err != nil || read == nil || s.put != nil {
		return read, err
	}
	s.put = make(chan error, 1)
	backend := make(chan int32, 1)
	go func() {
		s.put <- pgx.BeginFunc(context.Background(), s.pool, func(tx pgx.Tx) error {
			var pid int32
			if err := tx.QueryRow(context.Background(), "select pg_backend_pid()").Scan(&pid); err != nil {
				close(backend)
				return err
			}
			backend <- pid
			if err := intake.SerializeFacts(context.Background(), tx); err != nil {
				return err
			}
			return s.Store.PutHandover(context.Background(), tx, record.Handover{Issue: issue, Tree: read.Tree, Generation: read.Generation})
		})
	}()
	pid := <-backend
	for {
		select {
		case done := <-s.put:
			s.put <- done
			return read, nil
		default:
		}
		var waiting bool
		if err := s.pool.QueryRow(context.Background(), `select exists (select 1 from pg_locks
			where pid = $1 and locktype = 'advisory' and not granted)`, pid).Scan(&waiting); err != nil {
			return nil, fmt.Errorf("read whether the fact waits for the lock: %w", err)
		}
		if waiting {
			return read, nil
		}
		time.Sleep(5 * time.Millisecond)
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
