package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A health probe on a clock a test moves ends when the test moves that clock to the probe's bound,
// and not before, however long the probe waits on the wall clock: HealthyOn arms
// healthProbeTimeout on its clock, HealthyOnBy arms the moment its Deadline names there, and no
// wall-clock bound runs beneath either. The probe is held by a lock on schema_migrations, so only
// its bound can end it. A probe that armed its bound on the wall clock arms nothing on the fake,
// and one with a wall-clock bound beneath the fake's answers within healthProbeTimeout of wall time
// while the fake has not moved.
func TestHealthyOnEndsItsProbeAtTheBoundArmedOnItsClock(t *testing.T) {
	start := time.Date(2026, time.November, 28, 7, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		name  string
		bound time.Duration
		probe func(ctx context.Context, p *Pool, c clock.Clock) (int, error)
	}{
		{"HealthyOn arms healthProbeTimeout", healthProbeTimeout, func(ctx context.Context, p *Pool, c clock.Clock) (int, error) {
			return p.HealthyOn(ctx, c)
		}},
		{"HealthyOnBy arms its deadline's moment", 7 * time.Second, func(ctx context.Context, p *Pool, c clock.Clock) (int, error) {
			return p.HealthyOnBy(ctx, c, clock.DeadlineAt(start.Add(7*time.Second), true))
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			// A database of the row's own, so the lock it takes on schema_migrations holds no other row.
			database := openEmptyTestStore(t)
			ctx := testwait.Context(t)
			if err := database.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the lock's transaction: %v", err)
			}
			defer func() { _ = holder.Rollback(context.Background()) }()
			if _, err := holder.Exec(ctx, "lock table schema_migrations in access exclusive mode"); err != nil {
				t.Fatalf("lock schema_migrations: %v", err)
			}
			fake := clocktest.New(start)
			answered := make(chan error, 1)
			go func() {
				_, err := row.probe(ctx, database.Pool, fake)
				answered <- err
			}()
			due := start.Add(row.bound)
			fake.AwaitArmedAt(t, "the probe's bound on the fake", due)
			testwait.Until(t, "the probe's read to queue behind the lock on schema_migrations (the waiting reads last counted)", func() (int, bool) {
				var waiting int
				if err := holder.QueryRow(ctx, `
					select count(*) from pg_locks l join pg_stat_activity a using (pid)
					where not l.granted and a.datname = current_database() and a.query like 'select coalesce(max(version), 0)%'
				`).Scan(&waiting); err != nil {
					t.Fatalf("count the reads waiting on the lock: %v", err)
				}
				return waiting, waiting == 1
			})
			testwait.Quiet(t, "the probe's answer", answered, healthProbeTimeout+time.Second,
				"only the probe's bound can end a read the lock holds, and a wall-clock bound beneath the fake's would end it within healthProbeTimeout of wall time; no state shows a timer the probe did not arm on the fake")
			fake.Advance(row.bound - time.Nanosecond)
			if armed := fake.Armed(); len(armed) != 1 || !armed[0].Equal(due) {
				t.Fatalf("a nanosecond before the bound the fake holds %v; want the probe's bound %s alone, not yet fired", armed, due.Format(time.RFC3339Nano))
			}
			fake.Advance(time.Nanosecond)
			if err := testwait.Receive(t, "the probe's answer once the fake reaches its bound", answered); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the probe answered %v at its bound; want context.DeadlineExceeded", err)
			}
		})
	}
}

// A transaction is inside its xid window (InXidWindow) from the moment a test's statement tracer
// opens it (OpenXidWindow) until the transaction ends, on its context and every context derived
// from it, and nothing else opens one: a read, an empty batch and a sent batch leave it closed,
// so production, which installs no such tracer, never marks a transaction. The next transaction
// on the same context starts outside its window again.
func TestAnXidWindowIsOpenFromItsHookToTheEndOfItsTransaction(t *testing.T) {
	database := openEmptyTestStore(t)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := WithTransactionTracking(context.Background())
	if OpenXidWindow(ctx) || InXidWindow(ctx) {
		t.Fatal("a context that holds no connection had an xid window opened")
	}
	type derived struct{}
	for round := range 2 {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("round %d: begin: %v", round, err)
		}
		var one int
		if err := tx.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Fatalf("round %d: read: %v", round, err)
		}
		var empty WriteBatch
		if err := empty.Send(ctx, tx); err != nil {
			t.Fatalf("round %d: send an empty batch: %v", round, err)
		}
		if err := RunWrite(ctx, tx, func(b *WriteBatch) error {
			b.Queue("a write", "select 1")
			return nil
		}); err != nil {
			t.Fatalf("round %d: send a batch: %v", round, err)
		}
		if InXidWindow(ctx) {
			t.Fatalf("round %d: a transaction whose window no tracer opened is inside one", round)
		}
		if !OpenXidWindow(ctx) {
			t.Fatalf("round %d: the hook found no transaction to mark", round)
		}
		if !InXidWindow(ctx) || !InXidWindow(context.WithValue(ctx, derived{}, round)) {
			t.Fatalf("round %d: the transaction or a context derived from it is outside the window the hook opened", round)
		}
		end := tx.Commit
		if round == 1 {
			end = tx.Rollback
		}
		if err := end(ctx); err != nil {
			t.Fatalf("round %d: end the transaction: %v", round, err)
		}
		if InXidWindow(ctx) {
			t.Fatalf("round %d: a context whose transaction ended is still inside its xid window", round)
		}
	}
}
