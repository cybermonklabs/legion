package daemon

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// attemptAt claims row id as a lane claims a due row at at, runs it, and settles what it answered
// as the lane does (outbox.settle): a failure returns the row to the outbox on its backoff, with the
// failure as its last error, and a row that ran is finished. It answers what the row answered.
func (rig *taskBranchRig) attemptAt(t *testing.T, id int64, at time.Time) error {
	t.Helper()
	ctx := context.Background()
	var leased record.OutboxRow
	var claimed bool
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		leased, claimed, err = rig.records.ClaimRow(ctx, tx, id, at, time.Minute)
		return err
	}); err != nil || !claimed {
		t.Fatalf("claim outbox row %d at %s: claimed %t, %v", id, at, claimed, err)
	}
	err := rig.runner.execute(ctx, leased)
	rig.runner.settle(ctx, leased, err)
	return err
}

// joinFailedNotices is the reason of each task-join-failed notice the outbox holds, each with the
// role it reaches before it.
func (rig *taskBranchRig) joinFailedNotices(t *testing.T) []string {
	t.Helper()
	rows, err := rig.pool.Query(context.Background(), `select payload->>'role' || ': ' || (payload->>'reason') from outbox
		where kind = 'notice' and payload->>'kind' = 'task-join-failed' order by id`)
	var notices []string
	if err == nil {
		notices, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		t.Fatalf("read the task-join-failed notices: %v", err)
	}
	return notices
}

// A task's join holds the steps of that task alone, and a join that cannot end is told: task a
// holds the issue's branch and is at work, and d, which needs c, waits for its join at its first
// step. d's task_branch row is answered 500 and waits on its backoff, and the start of a's next
// step, queued after it, is due at once, while d's step keeps its wait. Then GitHub has no branch of
// c, so the row is answered 404 for it: its owner, the architect, is told once that d waits for the
// join of its need c, whose branch legion/LEGION-208.c GitHub lacks, and the row's next 404 tells
// nobody again. Once c's branch is on GitHub, the row's next try joins it and applies the end of the
// join.
//
// The faults it catches are one task's join that holds the starts of every task of the issue (its
// control: the start's fence on every older task_branch row of its issue, so a's start waits), and
// a join that cannot end, told to nobody (its control: the executor without its notice).
func TestAJoinHoldsOnlyItsOwnTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rig := newTaskBranchRig(t, plannedTask{id: "a"}, plannedTask{id: "c"}, plannedTask{id: "d", needs: []string{"c"}})
	var failing atomic.Bool
	rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !rig.github.Handler(w, r) {
			githubtest.Unexpected(t, w, r)
		}
	}})
	handler := &outboxFactHandler{}
	rig.runner.handlers, rig.runner.now = []intake.Handler{handler}, time.Now
	issue := taskBranchIssue
	queued := time.Now().UTC()
	var id int64
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		since := queued
		if err := rig.records.SetStepWait(ctx, tx, issue.Key, issue.Generation, "d", record.StepWaitJoin, &since); err != nil {
			return err
		}
		for _, payload := range []record.OutboxPayload{
			record.TaskBranch{Generation: issue.Generation, Task: "d"},
			record.SuperviseRequest{Op: "start", Tree: issue.Key, Role: claim.RoleReviewer, Generation: issue.Generation, Task: "Review task a."},
		} {
			row, err := record.NewOutboxRow(issue.Key, payload, queued)
			if err != nil {
				return err
			}
			if err := rig.records.Enqueue(ctx, tx, row); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `select id from outbox where kind = 'task_branch'`).Scan(&id)
	}); err != nil {
		t.Fatalf("queue d's task_branch row and a's start: %v", err)
	}

	failing.Store(true)
	if err := rig.attemptAt(t, id, queued); err == nil {
		t.Fatal("d's task_branch row answered 500 ran, want a failure")
	}
	var due []record.OutboxRow
	if err := store.Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		due, err = rig.records.DueRows(ctx, tx, "LEGION", issue.Key, nil, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("read the due rows of %s: %v", issue.Key, err)
	}
	var kinds []string
	for _, row := range due {
		kinds = append(kinds, string(row.Kind))
	}
	if !reflect.DeepEqual(kinds, []string{"supervise"}) {
		t.Fatalf("the due rows of %s while d's join waits on its backoff are of the kinds %v, want a's start alone: it waits for no join of d", issue.Key, kinds)
	}
	if wait := rig.task(t, "d").StepWait; wait != record.StepWaitJoin {
		t.Fatalf("d's step waits for %q, want its join", wait)
	}

	failing.Store(false)
	told := "architect: task d of LEGION-208 waits for the join of its needs: GitHub has no branch legion/LEGION-208.c of its need c. " +
		"Push that branch again, or stop the task with stop_task."
	for attempt := 1; attempt <= 2; attempt++ {
		if err := rig.attemptAt(t, id, queued.Add(time.Duration(attempt)*time.Hour)); err == nil || !strings.Contains(err.Error(), "legion/LEGION-208.c") {
			t.Fatalf("try %d of d's row while GitHub lacks c's branch answered %v, want a failure naming c's branch", attempt, err)
		}
		if notices := rig.joinFailedNotices(t); !reflect.DeepEqual(notices, []string{told}) {
			t.Fatalf("after try %d of d's row the task-join-failed notices = %q, want the one %q", attempt, notices, told)
		}
	}
	if facts := handler.facts(); len(facts) != 0 {
		t.Fatalf("a join that did not end applied %#v, want nothing", facts)
	}

	rig.github.Commit(t, rig.branch(t, "c"), map[string]string{"c.txt": "c\n"}, "c")
	if err := rig.attemptAt(t, id, queued.Add(3*time.Hour)); err != nil {
		t.Fatalf("d's row once c's branch is on GitHub: %v", err)
	}
	want := intake.TaskJoined{Issue: issue.Key, Generation: issue.Generation, Task: "d", Row: id, Branch: "legion/LEGION-208.d", Needs: []string{"c"}}
	if facts := handler.facts(); len(facts) != 1 || !reflect.DeepEqual(facts[0], want) {
		t.Fatalf("the facts d's row applied = %#v, want the one %#v", facts, want)
	}
}
