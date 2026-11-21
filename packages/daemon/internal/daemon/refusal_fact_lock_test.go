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
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A refused row's finish writes nothing a fact reads, so it never takes the lock every fact applies
// under (intake.SerializeFacts) and finishes while a fact holds it: here a status row Dispatch
// refused for good finishes within seconds, its notice queued and its row gone. The control is a
// fact applied on the same pool under the same held lock: it waits on the lock, and applies once
// the lock is free.
func TestARefusedRowFinishesWhileAFactHoldsTheFactLock(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	runner := &outbox{pool: pool, records: records, log: quietLogger(), handlers: []intake.Handler{engine}, now: time.Now}
	refusal := &dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no such issue"}
	ordinary := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-210",
		record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now().UTC()))

	fact := holdFactLock(t, pool)
	applied := make(chan error, 1)
	go func() {
		_, err := intake.ApplyFact(ctx, pool, "test", "probe", intake.ControllerTick{}, engine)
		applied <- err
	}()
	if !factLockWaited(pool) {
		t.Fatal("a fact applied while the lock is held never waited on it, so the finish below would prove nothing")
	}

	finished := make(chan error, 1)
	go func() { finished <- runner.finishRefused(ctx, ordinary, refusal) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("finish the refused status row: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused status row of LEGION-210 did not finish within 5s while a fact held the fact lock: its finish writes nothing a fact reads, so it has no fact to wait for")
	}
	if notices := noticesOf(t, pool, "LEGION-210"); notices != 1 {
		t.Fatalf("LEGION-210 has %d notices queued after its refused row finished, want the one that tells its architect", notices)
	}
	if rows := outboxRowsOfKindAnywhere(t, pool, record.OutboxKindDispatchStatus); rows != 0 {
		t.Fatalf("%d status rows are left after the refusal finished, want none", rows)
	}
	select {
	case err := <-applied:
		t.Fatalf("the control fact applied (%v) while the fact lock was held", err)
	default:
	}

	if err := fact.Rollback(ctx); err != nil {
		t.Fatalf("release the fact lock: %v", err)
	}
	select {
	case err := <-applied:
		if err != nil {
			t.Fatalf("apply the control fact: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the control fact did not apply within 10s once the fact lock was free")
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
// advisory lock: the fact lock is the only one the outbox takes, so a waiter is a transaction
// queued behind the fact that holds it.
func factLockWaited(pool *store.Pool) bool {
	return testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool {
		var waiting int
		err := pool.QueryRow(context.Background(), `select count(*) from pg_locks
			where locktype = 'advisory' and not granted
			  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting)
		return err == nil && waiting > 0
	})
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
