package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
