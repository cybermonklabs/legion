package workflow

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestCloses is each row of the outbox that closes pull requests (record.TaskPullRequestsClose),
// in the order it was queued.
func pullRequestCloses(t *testing.T, pool *pgxpool.Pool) []record.TaskPullRequestsClose {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload from outbox where kind = $1 order by id`, string(record.OutboxKindTaskPullRequestsClose))
	var payloads [][]byte
	if err == nil {
		payloads, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
	}
	if err != nil {
		t.Fatalf("read the rows that close pull requests: %v", err)
	}
	closes := make([]record.TaskPullRequestsClose, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &closes[i]); err != nil {
			t.Fatalf("decode a row that closes pull requests: %v", err)
		}
	}
	return closes
}

// pullRequestRows is how many rows of pull_requests hold the pull request number.
func pullRequestRows(t *testing.T, pool *pgxpool.Pool, number int) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), `select count(*) from pull_requests where number = $1`, number).Scan(&rows); err != nil {
		t.Fatalf("count the rows of pull request #%d: %v", number, err)
	}
	return rows
}

// A task that a later version removes closes its pull request, and its row goes in the version's
// transaction. Task a runs its tester's step on the issue's branch, b stopped at its implementer's
// step with its open pull request #2 on legion/LEGION-208.b, and z, the task that merges, waits on
// both. Version 2 removes b: one row closes #2 with the comment `Task b was removed from the plan.`,
// and the record holds no row of #2. Version 3 adds b back: b is a new task, which starts on
// legion/LEGION-208.b.2, a branch the generation gave to no task before. GitHub's close of #2,
// which the row asked for, finds no row, before b's implementer opens #3 there and after it: it
// moves no task, and writes no row of b and no outbox row; and once #3 is open, b's row holds #3.
//
// The fault it catches is a removed task's pull request left open on GitHub with its row in the
// record (its control: planGiven without the close and the delete, so no row closes #2 and #2's
// row stands). A fact of #2 reaches b added back only where b also took #2's branch again, since a
// row's task is read from its branch (record.PullRequest.Task); the
// generation gives a branch name once (record.Store.AssignTaskBranch). The row that reaches
// another task without that is the one on the issue's branch
// (TestARemovedTasksPullRequestOnTheIssuesBranchIsNoWaitingTasks).
func TestARemovedTaskClosesItsPullRequest(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"tester:a"}, agents: []claim.Agent{"test-a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskStopped, place: atStep(1)},
		planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208")
	var bBranch string
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		bBranch, err = record.NewStore().AssignTaskBranch(t.Context(), tx, root.Key, root.Generation, "b")
		return err
	})
	if bBranch != "legion/LEGION-208.b" {
		t.Fatalf("b's branch = %q, want legion/LEGION-208.b", bBranch)
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 2,
		Branch: bBranch, Base: "main", HeadSHA: "head-b", Failing: []string{}})
	apply := applyFacts(t, pool, engine)
	aBlock := keyed(t, "a", "", []string{"tester:a"})

	mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2,
		aBlock, keyed(t, "z", "", []string{"reviewer:z"}, "a"))))
	want := record.TaskPullRequestsClose{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{2}, Comment: "Task b was removed from the plan."}
	if closes := pullRequestCloses(t, pool); !reflect.DeepEqual(closes, []record.TaskPullRequestsClose{want}) {
		t.Fatalf("the rows that close pull requests after version 2 removed b = %+v, want the one %+v", closes, want)
	}
	if rows := pullRequestRows(t, pool, 2); rows != 0 {
		t.Fatalf("the record holds %d rows of b's pull request #2 after version 2 removed b, want none", rows)
	}

	mustTake(t, "version 3", apply("version-3", laterVersion(t, root.Key, root.Generation, 3,
		aBlock, keyed(t, "b", "", []string{"implementer:b"}), keyed(t, "z", "", []string{"reviewer:z"}, "a", "b"))))
	closeOf2 := func(id string, at time.Time, standing *record.PullRequest) {
		t.Helper()
		tasks, rows := tasksOf(t, pool, engine, root.Key), outboxRowsOf(t, pool, root.Key)
		mustSpend(t, "the close of #2", apply(id, intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 2, HeadSHA: "head-b", UpdatedAt: at}))
		if now := tasksOf(t, pool, engine, root.Key); !reflect.DeepEqual(now, tasks) {
			t.Fatalf("the tasks of %s after the close of #2:\n %+v\nwant them as they stood:\n %+v", root.Key, now, tasks)
		}
		var ofB *record.PullRequest
		seedRecord(t, pool, func(tx pgx.Tx) (err error) {
			ofB, err = record.NewStore().PullRequestOfTask(t.Context(), tx, root.Key, "b")
			return err
		})
		if !reflect.DeepEqual(ofB, standing) {
			t.Fatalf("b's pull request after the close of #2 = %+v, want it as it stood, %+v", ofB, standing)
		}
		if got := pullRequestRows(t, pool, 2); got != 0 {
			t.Fatalf("the record holds %d rows of #2 after its close, want none", got)
		}
		if now := outboxRowsOf(t, pool, root.Key); now != rows {
			t.Fatalf("%s holds %d outbox rows after the close of #2, want the %d it held before", root.Key, now, rows)
		}
	}
	closeOf2("closed-2", time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC), nil)
	again := tasksOf(t, pool, engine, root.Key)["b"]
	if again.State != record.TaskRunning || again.Branch != "legion/LEGION-208.b.2" {
		t.Fatalf("task b added back = %+v, want a new task running on legion/LEGION-208.b.2", again)
	}
	mustSpend(t, "the open of #3", apply("opened-3", intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 3, Branch: again.Branch, HeadRepo: "sjawhar/legion",
		Base: "main", HeadSHA: "head-b2", UpdatedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}))
	opened := pullRequestOfTask(t, pool, root.Key, "b")
	if opened.Number != 3 || opened.State != record.PullRequestOpen {
		t.Fatalf("the pull request of b added back = #%d %s, want the open #3", opened.Number, opened.State)
	}
	closeOf2("closed-2-again", time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC), &opened)
}

// The row of a removed task's pull request goes whatever its state, so no task reads it as its
// own. Task a holds the issue's branch, legion/LEGION-208, where its pull request #42 is open or
// closed, and stopped at its implementer's step; b runs its tester's step on legion/LEGION-208.b;
// z, the task that merges, waits on both, with no branch yet. Version 2 removes a: #42's row goes,
// so neither z's pull request nor the issue's merge reads #42, and the open #42 alone is closed,
// with the comment `Task a was removed from the plan.`.
//
// The fault it catches is a removed task's row on the issue's branch read as the pull request of a
// task that waits, since a task with no branch of its own holds the issue's branch while no task's
// row holds it (record.StoredPlan.TasksOfBranch), and so as the pull request of the issue's merge
// where that task merges (its controls: planGiven with the close but without the delete, where z
// reads #42 in both rows; and a delete of the open row alone, where z reads the closed #42).
func TestARemovedTasksPullRequestOnTheIssuesBranchIsNoWaitingTasks(t *testing.T) {
	for _, tc := range []struct {
		state  record.PullRequestState
		closes []record.TaskPullRequestsClose
	}{
		{record.PullRequestOpen, []record.TaskPullRequestsClose{{Generation: 1, Repo: "sjawhar/legion", Numbers: []int{42}, Comment: "Task a was removed from the plan."}}},
		{record.PullRequestClosed, []record.TaskPullRequestsClose{}},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Testing)
			seedPR(t, pool, record.PullRequest{State: tc.state, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskStopped, place: atStep(1)},
				planTask{id: "b", steps: []string{"tester:b"}, agents: []claim.Agent{"test-b"}, state: record.TaskRunning, place: atStep(1)},
				planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
			seedTaskBranch(t, pool, root.Key, "a", "legion/LEGION-208")
			seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
			apply := applyFacts(t, pool, engine)

			mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2,
				keyed(t, "b", "", []string{"tester:b"}), keyed(t, "z", "", []string{"reviewer:z"}, "b"))))
			if closes := pullRequestCloses(t, pool); !reflect.DeepEqual(closes, tc.closes) {
				t.Fatalf("the rows that close pull requests after version 2 removed a = %+v, want %+v", closes, tc.closes)
			}
			var ofZ, merge *record.PullRequest
			seedRecord(t, pool, func(tx pgx.Tx) (err error) {
				if ofZ, err = record.NewStore().PullRequestOfTask(t.Context(), tx, root.Key, "z"); err != nil {
					return err
				}
				merge, err = record.NewStore().MergePullRequest(t.Context(), tx, root.Key)
				return err
			})
			if ofZ != nil || merge != nil {
				t.Fatalf("after version 2 removed a, z's pull request = %+v and the issue's merge = %+v, want neither: #42 was a's", ofZ, merge)
			}
			if rows := pullRequestRows(t, pool, 42); rows != 0 {
				t.Fatalf("the record holds %d rows of a's pull request #42 after version 2 removed a, want none", rows)
			}
			if tasks := tasksOf(t, pool, engine, root.Key); len(tasks) != 2 || tasks["b"].Task != "b" || tasks["z"].Task != "z" {
				t.Fatalf("the tasks of %s after version 2 = %+v, want b and z", root.Key, tasks)
			}
		})
	}
}
