package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A refused row's finish takes the lock every fact applies under (intake.SerializeFacts) only
// when it drops a hand-over's fence, an input of the end of a merge's run that its transaction
// settles as a fact's does (settling). Every other refused row writes nothing a fact reads, so it
// finishes while a fact holds the lock: here an ordinary status row Dispatch refused for good
// finishes within seconds, its notice queued and its row gone. The control is the hand-over's
// refusal, on the same pool and under the same held lock: it waits on the lock, has not finished
// while the lock is held, and finishes and drops its fence once the lock is free.
func TestOnlyARefusedRowThatDropsAFenceWaitsOnTheFactLock(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	runner := lockProbeOutbox(pool, records)
	lingeringTree(t, pool, records, "LEGION-208", "LEGION-209", false)
	refusal := &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no such issue"}
	handOver := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-209",
		record.HandOverStatusWrite("todo", "in_progress", "LEGION-208", 1), time.Now().UTC()))
	ordinary := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-210",
		record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now().UTC()))

	fact := holdFactLock(t, pool)
	dropped := make(chan error, 1)
	go func() { dropped <- runner.finishRefused(ctx, handOver, refusal) }()
	if !factLockWaited(pool) {
		t.Fatal("the hand-over's refusal never waited on the fact lock, so the finish below would prove nothing")
	}

	finished := make(chan error, 1)
	go func() { finished <- runner.finishRefused(ctx, ordinary, refusal) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("finish the refused status row: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused status row of LEGION-210 did not finish within 5s while a fact held the fact lock: its finish drops no fence, so it has no fact to wait for")
	}
	if notices := noticesOf(t, pool, "LEGION-210"); notices != 1 {
		t.Fatalf("LEGION-210 has %d notices queued after its refused row finished, want the one that tells its architect", notices)
	}
	select {
	case err := <-dropped:
		t.Fatalf("the hand-over's refusal finished (%v) while a fact held the fact lock; its drop of the fence must wait for the fact", err)
	default:
	}

	if err := fact.Rollback(ctx); err != nil {
		t.Fatalf("release the fact lock: %v", err)
	}
	select {
	case err := <-dropped:
		if err != nil {
			t.Fatalf("finish the refused hand-over: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the hand-over's refusal did not finish within 10s once the fact lock was free")
	}
	if fence := fenceOf(t, pool, records, "LEGION-209"); fence != nil {
		t.Fatalf("LEGION-209's fence after its hand-over's refusal finished = %+v, want it dropped", fence)
	}
	if rows := outboxRowsOfKindAnywhere(t, pool, record.OutboxKindDispatchStatus); rows != 0 {
		t.Fatalf("%d status rows are left after both refusals finished, want none", rows)
	}
}

// The check of a hand-over (handOverHolds) reads the member, its fence and its root, and a read
// writes nothing a fact reads, so it takes no fact lock: while a fact holds the lock, the check of
// a fence that holds answers within seconds, and answers that it holds. The control is the check
// of a fence a re-admission cancelled, on the same pool and under the same held lock: it drops the
// fence, an input of the end of a merge's run, in a transaction that settles as a fact's does
// (settling), so it waits on the lock, has not answered while the lock is held, and answers that
// the hand-over no longer holds, its fence dropped, once the lock is free.
func TestTheCheckOfAHandOverWaitsOnTheFactLockOnlyToDropACancelledFence(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	runner := lockProbeOutbox(pool, records)
	lingeringTree(t, pool, records, "LEGION-208", "LEGION-209", false)
	lingeringTree(t, pool, records, "LEGION-300", "LEGION-301", true)
	live := record.HandOverStatusWrite("todo", "in_progress", "LEGION-208", 1)
	liveRow := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-209", live, time.Now().UTC()))
	cancelled := record.HandOverStatusWrite("todo", "in_progress", "LEGION-300", 1)
	cancelledRow := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-301", cancelled, time.Now().UTC()))

	fact := holdFactLock(t, pool)
	type answer struct {
		holds bool
		err   error
	}
	dropped := make(chan answer, 1)
	go func() {
		holds, err := runner.handOverHolds(ctx, cancelledRow, cancelled)
		dropped <- answer{holds, err}
	}()
	if !factLockWaited(pool) {
		t.Fatal("the check of the cancelled fence never waited on the fact lock, so the check below would prove nothing")
	}

	checked := make(chan answer, 1)
	go func() {
		holds, err := runner.handOverHolds(ctx, liveRow, live)
		checked <- answer{holds, err}
	}()
	select {
	case got := <-checked:
		if got.err != nil || !got.holds {
			t.Fatalf("the check of LEGION-209's hand-over holds %t, %v; want true: its fence is LEGION-208's linger 1 and the root lingers at 1", got.holds, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check of LEGION-209's hand-over did not answer within 5s while a fact held the fact lock: it only reads, so it has no fact to wait for")
	}
	select {
	case got := <-dropped:
		t.Fatalf("the check of the cancelled fence answered %t, %v while a fact held the fact lock; its drop of the fence must wait for the fact", got.holds, got.err)
	default:
	}

	if err := fact.Rollback(ctx); err != nil {
		t.Fatalf("release the fact lock: %v", err)
	}
	select {
	case got := <-dropped:
		if got.err != nil || got.holds {
			t.Fatalf("the check of the cancelled fence holds %t, %v; want false", got.holds, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the check of the cancelled fence did not answer within 10s once the fact lock was free")
	}
	if fence := fenceOf(t, pool, records, "LEGION-301"); fence != nil {
		t.Fatalf("LEGION-301's cancelled fence after its check = %+v, want it dropped", fence)
	}
	if fence := fenceOf(t, pool, records, "LEGION-209"); fence == nil || fence.Cancelled {
		t.Fatalf("LEGION-209's fence after its check = %+v, want it standing, not cancelled", fence)
	}
}

// lockProbeOutbox is an outbox over pool whose settle is the workflow engine's, as the daemon's
// is, so a transaction that settles runs the settle a fact runs.
func lockProbeOutbox(pool *store.Pool, records record.Store) *outbox {
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	return &outbox{pool: pool, records: records, log: quietLogger(), handlers: []intake.Handler{engine}, now: time.Now}
}

// lingeringTree records root as a done root lingering at generation 1 and member as its testing
// child, with member's hand-over fence on that linger, cancelled as a re-admission cancels it
// when cancelled is true.
func lingeringTree(t *testing.T, pool *store.Pool, records record.Store, root, member string, cancelled bool) {
	t.Helper()
	lingered := time.Now().Add(time.Hour)
	putOutboxIssue(t, pool, records, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U",
		LastDispatchSeq: 1, LingerUntil: &lingered})
	putOutboxIssue(t, pool, records, record.Issue{Key: member, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &root, Project: "LEGION", Title: "member", Phase: phase.Testing, Generation: 1,
		Status: "testing", Rank: "V", LastDispatchSeq: 1})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutHandover(context.Background(), tx, record.Handover{Issue: member, Tree: root, Generation: 1}); err != nil {
			return err
		}
		if !cancelled {
			return nil
		}
		return records.CancelTreeHandovers(context.Background(), tx, root, 1)
	}); err != nil {
		t.Fatalf("seed %s's fence on %s's linger: %v", member, root, err)
	}
}

// holdFactLock takes the lock every fact applies under in a transaction of the test's own, as a
// long fact holds it, and rolls that transaction back when the test ends.
func holdFactLock(t *testing.T, pool *store.Pool) pgx.Tx {
	t.Helper()
	fact, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the fact's transaction: %v", err)
	}
	t.Cleanup(func() { _ = fact.Rollback(context.Background()) })
	if err := intake.SerializeFacts(context.Background(), fact); err != nil {
		t.Fatalf("take the fact lock: %v", err)
	}
	return fact
}

// factLockWaited is whether, within ten seconds, a backend of pool's database waits on an
// advisory lock: the fact lock is the only one the outbox takes, so a waiter is an outbox
// transaction queued behind the fact that holds it.
func factLockWaited(pool *store.Pool) bool {
	return testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool {
		var waiting int
		err := pool.QueryRow(context.Background(), `select count(*) from pg_locks
			where locktype = 'advisory' and not granted
			  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting)
		return err == nil && waiting > 0
	})
}

// fenceOf is issue's hand-over fence as the record holds it, nil when it has none.
func fenceOf(t *testing.T, pool *store.Pool, records record.Store, issue string) *record.Handover {
	t.Helper()
	var fence *record.Handover
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		fence, err = records.Handover(context.Background(), tx, issue)
		return err
	}); err != nil {
		t.Fatalf("read %s's fence: %v", issue, err)
	}
	return fence
}

// noticesOf is how many notice rows are queued for issue.
func noticesOf(t *testing.T, pool *store.Pool, issue string) int {
	t.Helper()
	var notices int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1 and kind = $2",
		issue, string(record.OutboxKindNotice)).Scan(&notices); err != nil {
		t.Fatalf("count the notices of %s: %v", issue, err)
	}
	return notices
}

// outboxRowsOfKindAnywhere is how many rows of kind the outbox holds, of every issue.
func outboxRowsOfKindAnywhere(t *testing.T, pool *store.Pool, kind record.OutboxKind) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = $1", string(kind)).Scan(&rows); err != nil {
		t.Fatalf("count the %s rows: %v", kind, err)
	}
	return rows
}
