package admit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stepWrites is every step record row issue's outbox holds, in the order they were written.
func stepWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.StepRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindStepRecord), issue)
	if err != nil {
		t.Fatalf("read the step records of %s: %v", issue, err)
	}
	defer rows.Close()
	var got []record.StepRecord
	for rows.Next() {
		var write record.StepRecord
		if err := rows.Scan(&write); err != nil {
			t.Fatalf("decode a step record of %s: %v", issue, err)
		}
		got = append(got, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the step records of %s: %v", issue, err)
	}
	return got
}

// stepRun is one write of a step record as these tests name it: a start (end "") or an end with
// its result, of kind and step of the task default, in generation, under the count of starts its
// run's id is computed from (workflow.StepID).
type stepRun struct {
	end        string
	kind       string
	step       int
	generation uint64
	starts     int64
}

func start(kind string, step int, generation uint64, starts int64) stepRun {
	return stepRun{kind: kind, step: step, generation: generation, starts: starts}
}

func end(result, kind string, step int, generation uint64, starts int64) stepRun {
	return stepRun{end: result, kind: kind, step: step, generation: generation, starts: starts}
}

// assertStepWrites fails unless key's step record rows are exactly want, in order.
func assertStepWrites(t *testing.T, pool *pgxpool.Pool, key string, want ...stepRun) {
	t.Helper()
	got := stepWrites(t, pool, key)
	var lines []string
	for _, w := range got {
		result := ""
		if w.Result != nil {
			result = *w.Result
		}
		lines = append(lines, fmt.Sprintf("%s %s/%d generation %d ended %t result %q", w.ID, w.Kind, w.Step, w.Generation, w.EndedAt != nil, result))
	}
	fail := func(why string) {
		t.Helper()
		t.Fatalf("%s: the step records of %s are\n\t%s\nwant %+v", why, key, strings.Join(lines, "\n\t"), want)
	}
	if len(got) != len(want) {
		fail(fmt.Sprintf("%d rows, want %d", len(got), len(want)))
	}
	for i, w := range want {
		g := got[i]
		result := ""
		if g.Result != nil {
			result = *g.Result
		}
		if g.ID != workflow.StepID(key, w.generation, record.StepPlace{Task: "default", Kind: w.kind, Place: w.step}, w.starts) || g.Kind != w.kind || g.Step != w.step ||
			g.Generation != w.generation || (g.EndedAt != nil) != (w.end != "") || result != w.end {
			fail(fmt.Sprintf("row %d is not %+v", i, w))
		}
	}
}

// countStart writes the start of the step key stands in, as the transition or the promotion that
// started it would have (workflow.Engine.StartStepRecord), in a fact of its own: its count raised
// and its start queued.
func countStart(t *testing.T, pool *pgxpool.Pool, engine intake.Handler, key string) {
	t.Helper()
	stored := issue(t, pool, key)
	workflowEngine := engine.(*workflow.Engine)
	ctx := workflowEngine.OpenFact(context.Background())
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflowEngine.StartStepRecord(ctx, tx, stored); err != nil {
			t.Fatalf("start the step of %s: %v", key, err)
		}
		if err := workflowEngine.SettleFact(ctx, tx); err != nil {
			t.Fatalf("settle the start of %s: %v", key, err)
		}
	})
}

// closeFinishingTree parks and closes the finishing tree's root (seedFinishingTree), a person's
// close: the root leaves Legion's work, and nothing under it is stopped by it.
func closeFinishingTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler) {
	t.Helper()
	apply(t, pool, admission, "root-parked", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: finishedRoot, ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.closed", Status: "done", Title: finishedRoot, Rank: "A", HandedOver: true}, engine)
}

// seedAwaitingPullRequest gives the finishing tree's member in merging its open pull request #5.
func seedAwaitingPullRequest(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: awaiting, Repo: "acme/widgets", Number: 5,
			Branch: "legion/LEGION-5", HeadSHA: "head-5", Generation: 1, State: record.PullRequestOpen}); err != nil {
			t.Fatalf("seed the pull request of %s: %v", awaiting, err)
		}
	})
}

// awaitingFirstRead is when the first read conflictReadOfAwaiting records found head-5
// conflicting: two minutes before fixedNow. A second read's row names it
// (record.ConflictConfirm.FirstReadAt), and so does the fact the row applies at its end.
var awaitingFirstRead = fixedNow.Add(-2 * time.Minute)

// conflictReadOfAwaiting gives the member in merging its open pull request #5 onto main with a
// first read that found head-5 conflicting at awaitingFirstRead, the state a second read of that
// head applies on (dispatch://INFRA-4374 R2). Its arguments are a row's arrange's.
func conflictReadOfAwaiting(t *testing.T, pool *pgxpool.Pool, _ *Admission, _ intake.Handler) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{Issue: awaiting, Repo: "acme/widgets", Number: 5,
			Branch: "legion/LEGION-5", Base: "main", HeadSHA: "head-5", Generation: 1, State: record.PullRequestOpen, Failing: []string{},
			ConflictRead: &record.ConflictRead{Head: "head-5", Base: "main", At: awaitingFirstRead}}); err != nil {
			t.Fatalf("seed the pull request of %s with its first conflicting read: %v", awaiting, err)
		}
	})
}
