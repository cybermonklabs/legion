package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// One row of a close_read and one of a close_wait_publish stand for an issue at a time, so their
// writers ask the outbox whether a row of that kind is still queued for the issue
// (OutboxQueued). It answers false where none is, true once one is queued, and false for a row of
// another issue or of another kind. BranchQueued is its read of the issue_branch kind.
func TestOutboxQueuedSaysWhetherARowOfAKindStandsForAnIssue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	queued := func(issue string, kind OutboxKind) bool {
		t.Helper()
		var got bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.OutboxQueued(ctx, tx, issue, kind)
			must(t, err)
		})
		return got
	}
	if queued("ACME-1", OutboxKindCloseRead) {
		t.Fatal("a close_read row of ACME-1 reads as queued with the outbox empty")
	}

	now := time.Now()
	inTx(t, st, func(tx pgx.Tx) {
		for _, row := range []struct {
			issue   string
			payload OutboxPayload
		}{
			{"ACME-1", CloseRead{Generation: 1}},
			{"ACME-2", CloseWaitPublish{Generation: 1}},
		} {
			queuedRow, err := NewOutboxRow(row.issue, row.payload, now)
			must(t, err)
			must(t, records.Enqueue(ctx, tx, queuedRow))
		}
	})
	for _, tc := range []struct {
		issue string
		kind  OutboxKind
		want  bool
	}{
		{"ACME-1", OutboxKindCloseRead, true},
		{"ACME-1", OutboxKindCloseWaitPublish, false},
		{"ACME-2", OutboxKindCloseRead, false},
		{"ACME-2", OutboxKindCloseWaitPublish, true},
		{"ACME-3", OutboxKindCloseRead, false},
	} {
		if got := queued(tc.issue, tc.kind); got != tc.want {
			t.Errorf("a %s row of %s reads as queued = %t, want %t", tc.kind, tc.issue, got, tc.want)
		}
	}
}
