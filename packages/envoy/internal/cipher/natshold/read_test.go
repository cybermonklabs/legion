package natshold_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/natshold"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// testPool is a pool on a migrated schema of the test's own, and that schema's name, which the
// hold's read names its table by.
func testPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := storetest.URL(t)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, parsed.Query().Get("search_path")
}

// The read answers no row as not found, a stored row as it is stored, and an error for a table it
// cannot read: every one of those is a reading the auth service must tell apart.
func TestTheHoldReadAnswersTheStoredRow(t *testing.T) {
	pool, schema := testPool(t)
	ctx := context.Background()
	if _, found, err := natshold.Read(ctx, pool, schema); err != nil || found {
		t.Fatalf("Read of an empty table = found %v, %v; want not found and no error", found, err)
	}
	signedAt := time.Date(2026, 11, 20, 1, 2, 3, 4000, time.UTC)
	if _, err := pool.Exec(ctx, `insert into nats_hold (id, gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
		values (1, 7, 'held', 'h-1', 'r', now(), 'operator', $1, 'c2ln')`, signedAt); err != nil {
		t.Fatal(err)
	}
	got, found, err := natshold.Read(ctx, pool, schema)
	if err != nil || !found {
		t.Fatalf("Read of a stored row = found %v, %v", found, err)
	}
	want := natshold.Reading{Gen: 7, Phase: "held", HoldID: "h-1", SignedAt: signedAt, Sig: "c2ln"}
	if !got.SignedAt.Equal(want.SignedAt) || got.Gen != want.Gen || got.Phase != want.Phase || got.HoldID != want.HoldID || got.Sig != want.Sig {
		t.Fatalf("Read = %+v, want %+v", got, want)
	}
	if _, _, err := natshold.Read(ctx, pool, schema+"_missing"); err == nil {
		t.Fatal("Read of a schema with no hold table answered no error")
	}
}

// The read takes its own deadline, ReadDeadline, over the connection's acquire and the whole
// transaction, whatever deadline its caller's context carries: a read held behind a lock on the row
// fails within it rather than waiting.
func TestTheHoldReadEndsAtItsDeadline(t *testing.T) {
	pool, schema := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `insert into nats_hold (id, gen, phase, hold_id, reason, updated_at, updated_by, signed_at, sig)
		values (1, 1, 'released', 'h', 'r', now(), 'operator', now(), 'c2ln')`); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `lock table nats_hold in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, _, err = natshold.Read(ctx, pool, schema)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a read held behind an exclusive lock answered no error")
	}
	if elapsed > natshold.ReadDeadline+250*time.Millisecond {
		t.Fatalf("a read held behind an exclusive lock took %s, want at most its deadline of %s", elapsed, natshold.ReadDeadline)
	}
}
