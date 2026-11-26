package refs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// A backoff is written and judged on the database's clock wherever the caller passes no moment,
// as the reconciliation does in production: DeferExcerpts stamps the database's now() of the
// statement that defers, DriftedExcerpts leaves the document out until that clock reaches its
// retry, and NextExcerptRetry measures the wait on it, so no process's clock, however far it is off
// the database's, moves a retry. The statements run in one transaction, whose now() is the moment
// each of them reads, so the test reads that moment exactly. Given a moment, as a test's fake
// gives, each statement reads it in place of now(), and the retry is held exactly to its due time
// on that moment.
func TestABackoffIsWrittenAndJudgedOnTheDatabasesClock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := owedDocument(t, database, "skewed")
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := testdb.Now(t, tx)
	due := func() time.Time {
		t.Helper()
		var at time.Time
		if err := tx.QueryRow(ctx, `select next_attempt_at from reference_excerpt_backoff where artifact_id = $1::uuid`, document).Scan(&at); err != nil {
			t.Fatalf("read the document's retry: %v", err)
		}
		return at
	}
	listed := func(at *time.Time) bool {
		t.Helper()
		drifted, err := DriftedExcerpts(ctx, tx, 20, at)
		if err != nil {
			t.Fatalf("list the drift: %v", err)
		}
		return len(drifted) == 1 && drifted[0].FromID == document
	}

	// With no backoff, a deferral's retry is its own moment: the stamp is the clock's reading.
	if err := DeferExcerpts(ctx, tx, document, 1, 0, 0, nil); err != nil {
		t.Fatalf("defer with no moment: %v", err)
	}
	if got := due(); !got.Equal(now) {
		t.Fatalf("a deferral given no moment and no backoff is due at %v, want the database's now(), %v: the stamp is another clock's", got, now)
	}
	if !listed(nil) {
		t.Fatal("a document due at the database's now() is left out of the drift judged at that now()")
	}
	if _, err := tx.Exec(ctx, `update reference_excerpt_backoff set next_attempt_at = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if listed(nil) {
		t.Fatal("a document due an hour past the database's now() is listed in the drift judged at that now()")
	}
	if wait, owed, err := NextExcerptRetry(ctx, tx, nil); err != nil || !owed || wait != time.Hour {
		t.Fatalf("next retry judged at the database's now() = %v (owed %t), %v; want an hour", wait, owed, err)
	}

	// On a moment of the caller's, years off the database's clock, as a test's fake gives: the
	// retry, a minute and up to a fifth more after it, is held exactly to its due time there.
	moment := now.Add(-10 * 365 * 24 * time.Hour)
	if err := DeferExcerpts(ctx, tx, document, 1, time.Minute, time.Minute, &moment); err != nil {
		t.Fatalf("defer at a moment: %v", err)
	}
	retry := due()
	if wait := retry.Sub(moment); wait < time.Minute || wait > time.Minute+time.Minute/5 {
		t.Fatalf("a deferral at a moment is due %v after it, want a minute and at most a fifth more", wait)
	}
	early := retry.Add(-time.Microsecond)
	if listed(&early) {
		t.Fatal("the document is listed a microsecond before its retry on the caller's moment")
	}
	if !listed(&retry) {
		t.Fatal("the document is left out at its retry on the caller's moment")
	}
	if wait, owed, err := NextExcerptRetry(ctx, tx, &moment); err != nil || !owed || wait != retry.Sub(moment) {
		t.Fatalf("next retry judged at the moment = %v (owed %t), %v; want %v", wait, owed, err, retry.Sub(moment))
	}
}
