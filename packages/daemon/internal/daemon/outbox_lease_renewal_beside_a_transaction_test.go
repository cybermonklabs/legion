package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A running row's lease is renewed while the row's own effect is inside a transaction of the
// pool. The renewal's tick runs beside the row, on a timer goroutine of its own (renewLease), so
// it is a second caller of the pool and not a second connection of the row's: it must be served
// its own connection, as it was before the pool refused nested connections (INFRA-3169).
//
// Here the row's read of its hand-over (handOverHolds) waits on a lock the test holds, which is
// the shape INFRA-3169 measured: a transaction that waits while it holds its connection, as a fact
// that queues on the fact serializer does. A tick that comes in that wait has to move the lease a
// whole lease ahead, as it does for a row blocked outside any transaction
// (TestALaneRenewsItsRunningRowsLeaseAndNoSweepClaimsItAgain). A tick refused instead leaves the
// lease where it was, and three refused ticks in a row lapse it while the database answers, so
// the sweep runs the row a second time beside the first.
func TestALaneRenewsItsRowsLeaseWhileTheRowIsInsideATransaction(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	clock := microClock()
	ctx := context.Background()
	const issue = "LEGION-208"
	// The issue the hand-over's read finds first, so the read goes on to the hand-over table the
	// test locks.
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, runningRoot(issue, phase.Implementing))
	}); err != nil {
		t.Fatalf("record the issue the hand-over's read finds: %v", err)
	}
	client := &blockingDispatch{issue: dispatch.Issue{Key: issue, Status: "todo"}}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, log: quietLogger()}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue,
		record.HandOverStatusWrite("todo", "todo", "LEGION-1", 1), clock.Now()))

	lock, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	var unlocked sync.Once
	unlock := func() { unlocked.Do(func() { _ = lock.Rollback(context.Background()) }) }
	t.Cleanup(unlock)
	if _, err := lock.Exec(ctx, "lock table handovers in access exclusive mode"); err != nil {
		t.Fatalf("lock the hand-over table against the row's read: %v", err)
	}
	runLanes(t, runner, clock, unlock)

	awaitOneLockWaiter(t, pool, "the row's read of its hand-over to wait on the test's lock")
	id := outboxIDs(t, pool)[0]
	claimed := rowStateOf(t, pool, id)
	if claimed.leaseToken == "" || claimed.leaseUntil == nil {
		t.Fatalf("the running row is leased %q until %v, want it the lane's", claimed.leaseToken, claimed.leaseUntil)
	}

	renewEvery := outboxLease / 3
	clock.advance(renewEvery)
	if fired := clock.fire(renewEvery); fired != 1 {
		t.Fatalf("renewal timers fired = %d, want the running row's", fired)
	}
	renewed := rowStateOf(t, pool, id)
	if renewed.leaseUntil == nil || !renewed.leaseUntil.Equal(clock.Now().Add(outboxLease)) {
		t.Fatalf("lease_until = %v after the tick, want %v, a whole lease ahead of now: the tick was not served while the row held its transaction",
			renewed.leaseUntil, clock.Now().Add(outboxLease))
	}
	if renewed.leaseToken != claimed.leaseToken {
		t.Fatalf("lease token %q after the tick, want the row's own %q", renewed.leaseToken, claimed.leaseToken)
	}

	unlock()
	testwait.Eventually(t, "the row finished once the lock went", func() bool { return outboxRows(t, pool) == 0 })
}

// A renewal tick that finds its row no longer under its lease says why, from the row it runs. A row
// another lane leases is a lost lease, at Warn. A left pod's close check (record.IssueWorkspaceCheck
// with CloseRoles) finishes its own row inside its run (checkLeftPodWorkspace), so that row found
// finished is logged as that, at Info. The idle release's check is the same kind and never finishes
// its own row, so that one found gone was finished by another lane after this lease lapsed, at
// Warn. A status write found gone may also have been dropped by a newer write of its issue
// (record's DropStatusWrites, which deletes a leased row), at Warn, and the line says so. Each
// finish or drop here runs as it does in a daemon: in a transaction that holds the row while the
// tick's renewal waits on its lock, and commits under it, so the tick reads the row gone only by the
// statement after its update (record's RenewOutboxLease).
func TestALeaseRenewalThatFindsItsRowGoneSaysWhy(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	now := time.Now().UTC()
	rows := []record.OutboxRow{
		mustOutboxRow(t, "LEGION-208", record.HandOverStatusWrite("todo", "todo", "LEGION-1", 1), now),
		mustOutboxRow(t, "LEGION-209", record.IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7,
			Role: claim.RolePlanner, CloseRoles: []claim.Role{claim.RolePlanner}}, now),
		mustOutboxRow(t, "LEGION-210", record.IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7,
			Role: claim.RoleImplementer}, now),
	}
	for _, row := range rows {
		enqueueOutbox(t, pool, records, row)
	}
	for i, id := range outboxIDs(t, pool) {
		rows[i].ID, rows[i].LeaseToken = id, "this-run"
	}
	status, closeCheck, idleCheck := rows[0], rows[1], rows[2]
	logs, log := watchedLog()
	runner := &outbox{pool: pool, records: records, log: log}
	lease := func(row record.OutboxRow, token string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `update outbox set lease_token = $2, lease_until = now() + interval '5 minutes'
			where id = $1`, row.ID, token); err != nil {
			t.Fatalf("lease row %d to %s: %v", row.ID, token, err)
		}
	}
	tick := func(row record.OutboxRow) {
		runner.renewOnce(ctx, row, &leaseRenewal{warned: map[int64]bool{}}, now)
	}
	// goneUnderTick deletes row by remove, in a transaction of its own, and runs one tick while that
	// transaction holds the row, committing it once the tick's renewal waits on its lock.
	goneUnderTick := func(row record.OutboxRow, remove func(tx pgx.Tx) error) {
		t.Helper()
		lease(row, row.LeaseToken)
		holding, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin the transaction that deletes row %d: %v", row.ID, err)
		}
		defer func() { _ = holding.Rollback(context.Background()) }()
		if err := remove(holding); err != nil {
			t.Fatalf("delete row %d: %v", row.ID, err)
		}
		ticked := make(chan struct{})
		go func() {
			tick(row)
			close(ticked)
		}()
		awaitOneLockWaiter(t, pool, "the tick's renewal to wait on the delete's lock")
		if err := holding.Commit(ctx); err != nil {
			t.Fatalf("commit the delete of row %d: %v", row.ID, err)
		}
		select {
		case <-ticked:
		case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
			t.Fatalf("the tick over row %d did not end once the delete committed", row.ID)
		}
	}
	finish := func(row record.OutboxRow) func(pgx.Tx) error {
		return func(tx pgx.Tx) error { return records.FinishLeasedOutbox(ctx, tx, row.ID, row.LeaseToken) }
	}
	lines := map[string]string{
		"lost":     "level=WARN msg=\"outbox row lease lost",
		"finished": "level=INFO msg=\"outbox row finished while its run goes on",
		"lapsed":   "level=WARN msg=\"outbox row gone while its run goes on: another lane finished it after this lease lapsed",
		"dropped":  "level=WARN msg=\"outbox row gone while its run goes on: a newer write of its issue may have dropped it",
	}
	want := func(when string, counts map[string]int) {
		t.Helper()
		for name, line := range lines {
			if got := count(logs, line); got != counts[name] {
				t.Errorf("%s: %s lines = %d, want %d: %s", when, name, got, counts[name], logs.String())
			}
		}
	}

	lease(status, "another-lane")
	tick(status)
	want("after a tick that found another lane's lease", map[string]int{"lost": 1})
	goneUnderTick(closeCheck, finish(closeCheck))
	want("after a tick that found the close's check finished", map[string]int{"lost": 1, "finished": 1})
	goneUnderTick(idleCheck, finish(idleCheck))
	want("after a tick that found the idle release's check finished", map[string]int{"lost": 1, "finished": 1, "lapsed": 1})
	goneUnderTick(status, func(tx pgx.Tx) error { return records.DropStatusWrites(ctx, tx, status.Issue) })
	want("after a tick that found the status write dropped", map[string]int{"lost": 1, "finished": 1, "lapsed": 1, "dropped": 1})
}
