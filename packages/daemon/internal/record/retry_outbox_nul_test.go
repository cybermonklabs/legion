package record

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// A failed attempt's error is the far side's own text, and PostgreSQL refuses a NUL and a byte
// that is not UTF-8 in text (SQLSTATE 22021): a retry that stored it whole would be refused, and
// the row's attempts and next time would never move. The row takes the bounded far-side text
// instead: valid UTF-8 with U+FFFD for each NUL and each invalid byte, its first words kept, and
// cut to 2048 runes ending with an ellipsis, since a relayed command's whole stderr runs to a
// megabyte.
func TestRetryOutboxStoresAnInvalidFarSideErrorBoundedTo2048Runes(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	var id int64
	inTx(t, st, func(tx pgx.Tx) {
		var err error
		id, err = records.EnqueueID(ctx, tx, OutboxRow{Kind: OutboxKindDispatchStatus, Issue: "LEGION-208",
			Payload: json.RawMessage(`{"status":"testing"}`), NextAt: now})
		must(t, err)
	})
	var leased OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		row, ok, err := records.ClaimRow(ctx, tx, id, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("outbox row %d was not claimable at %s", id, now)
		}
		leased = row
	})
	const first = "the far side answered 502 Bad Gateway: \x00 upstream reset \xff\n"
	const line = "the relayed command's stderr goes on, line after line.\n"
	lastErr := first + strings.Repeat(line, (1<<20)/len(line)+1)
	nextAt := now.Add(5 * time.Minute)

	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return records.RetryOutbox(ctx, tx, id, leased.LeaseToken, nextAt, lastErr)
	}); err != nil {
		t.Fatalf("RetryOutbox with the far side's error of %d bytes: %v", len(lastErr), err)
	}

	retried, err := scanOutbox(st.Pool().QueryRow(ctx, "select "+outboxColumns+" from outbox where id = $1", id))
	if err != nil {
		t.Fatalf("read outbox row %d: %v", id, err)
	}
	if retried.Attempts != leased.Attempts+1 {
		t.Errorf("outbox row %d has %d attempts after the retry, want the %d it had plus one", id, retried.Attempts, leased.Attempts)
	}
	if !retried.NextAt.Equal(nextAt) {
		t.Errorf("outbox row %d is next due at %s, want the %s the retry gave", id, retried.NextAt, nextAt)
	}
	got := retried.LastError
	if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
		t.Fatalf("the stored last error starts %q, want valid UTF-8 with no NUL", got[:min(len(got), 64)])
	}
	if want := "the far side answered 502 Bad Gateway: \uFFFD upstream reset \uFFFD\n"; !strings.HasPrefix(got, want) {
		t.Errorf("the stored last error starts %q, want the error's first words %q", got[:min(len(got), len(want))], want)
	}
	if runes := utf8.RuneCountInString(got); runes != 2048 || !strings.HasSuffix(got, "…") {
		t.Errorf("the stored last error is %d runes ending %q, want 2048 runes ending with …", runes, got[max(0, len(got)-16):])
	}
}
