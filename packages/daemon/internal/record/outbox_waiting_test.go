package record

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// One close_read and one close_wait_publish row wait for an issue at a time, and each reads the
// record when it runs. Their writers ask for the row of the kind that still waits (OutboxWaiting):
// none where there is none, the row itself where no lane holds it, and none for a row a lane runs
// now under a live lease, since the close read applies its fact while its own row is leased and
// that fact can owe the next read. A row whose lane died mid-run, whose lease lapsed, waits again:
// the lane runs it again from the start. Another issue's row and another kind's row answer none.
// The row answered is the waiting row itself, with its id and its next_at, which is what a caller
// that makes the read it asks for due (ExpediteOutbox) names.
func TestOutboxWaitingCountsOnlyARowNoLiveLeaseHolds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	waiting := func(issue string, kind OutboxKind) (OutboxRow, bool) {
		t.Helper()
		var row OutboxRow
		var got bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			row, got, err = records.OutboxWaiting(ctx, tx, issue, kind)
			must(t, err)
		})
		return row, got
	}
	lease := func(until string) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			tag, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() + $1::interval
				where issue = 'ACME-1' and kind = $2`, until, string(OutboxKindCloseRead))
			must(t, err)
			if tag.RowsAffected() != 1 {
				t.Fatalf("leased %d close_read rows of ACME-1, want the one", tag.RowsAffected())
			}
		})
	}
	if _, got := waiting("ACME-1", OutboxKindCloseRead); got {
		t.Fatal("a close_read row of ACME-1 waits with the outbox empty")
	}

	due := time.Now().UTC().Truncate(time.Microsecond)
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("ACME-1", CloseRead{Generation: 1}, due)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	for _, tc := range []struct {
		issue string
		kind  OutboxKind
		want  bool
	}{
		{"ACME-1", OutboxKindCloseRead, true},
		{"ACME-1", OutboxKindCloseWaitPublish, false},
		{"ACME-2", OutboxKindCloseRead, false},
	} {
		if _, got := waiting(tc.issue, tc.kind); got != tc.want {
			t.Errorf("a %s row of %s with one close_read of ACME-1 queued and unleased waits = %t, want %t", tc.kind, tc.issue, got, tc.want)
		}
	}
	if row, _ := waiting("ACME-1", OutboxKindCloseRead); row.ID == 0 || row.Issue != "ACME-1" ||
		row.Kind != OutboxKindCloseRead || !row.NextAt.Equal(due) {
		t.Errorf("the waiting row answered = %+v, want the close_read row of ACME-1 queued for %s, with its id", row, due)
	}

	lease("5 minutes")
	if _, got := waiting("ACME-1", OutboxKindCloseRead); got {
		t.Error("the close_read row of ACME-1 a lane runs now under a live lease waits, want it not waiting: the fact its run applies could never queue the next read")
	}
	lease("-5 minutes")
	if _, got := waiting("ACME-1", OutboxKindCloseRead); !got {
		t.Error("the close_read row of ACME-1 whose lease lapsed does not wait, want it waiting: its lane runs it again from the start")
	}
}

// A writer that finds a row waiting (OutboxWaiting) queues none, so it relies on that row running
// after its own change commits: the row reads the record when it runs. So the read holds the row
// it answers until the writer's transaction ends, and a lane's claim of it (ClaimRow) waits for
// that commit. A claim that committed between the read and the writer's commit ran the row on the
// record before the change and finished it, and the change had no row: close-wait.md kept the
// version before the change until the wait changed again.
func TestARowOutboxWaitingAnswersIsClaimedOnlyOnceItsWriterEnds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	due := time.Now().UTC().Truncate(time.Microsecond)
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("ACME-1", CloseWaitPublish{Generation: 1}, due)
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	writer, err := st.Pool().BeginTx(ctx, pgx.TxOptions{})
	must(t, err)
	defer func() { _ = writer.Rollback(ctx) }()
	waiting, found, err := records.OutboxWaiting(ctx, writer, "ACME-1", OutboxKindCloseWaitPublish)
	must(t, err)
	if !found {
		t.Fatal("the close_wait_publish row of ACME-1, queued and unleased, does not wait")
	}
	claimHeldBy(t, st, waiting.ID, "the writer that found it waiting")
	must(t, writer.Commit(ctx))
	if claimed, err := claimRow(st, waiting.ID); err != nil || !claimed {
		t.Fatalf("a lane's claim of row %d once its writer committed = claimed %t, error %v; want it claimed", waiting.ID, claimed, err)
	}
}

// claimRow is a lane's claim of row id (ClaimRow) on a connection of its own, which waits at most
// a second for the row's lock: whether it claimed the row, and the claim's error.
func claimRow(st *store.Store, id int64) (bool, error) {
	var claimed bool
	err := store.Transact(context.Background(), st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "set local lock_timeout = '1s'"); err != nil {
			return err
		}
		var err error
		_, claimed, err = NewStore().ClaimRow(ctx, tx, id, time.Now().UTC(), time.Minute)
		return err
	})
	return claimed, err
}

// claimHeldBy fails the test unless a lane's claim of row id waits for the row's lock, which
// holder holds, until the lock's timeout refuses it (SQLSTATE 55P03).
func claimHeldBy(t *testing.T, st *store.Store, id int64, holder string) {
	t.Helper()
	claimed, err := claimRow(st, id)
	var refused *pgconn.PgError
	if !errors.As(err, &refused) || refused.Code != "55P03" {
		t.Fatalf("a lane's claim of row %d while %s is open = claimed %t, error %v; want it to wait for the row's lock until the timeout (SQLSTATE 55P03): a claim before that writer commits runs the row on the record before its change",
			id, holder, claimed, err)
	}
}
