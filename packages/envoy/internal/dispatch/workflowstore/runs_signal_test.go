package workflowstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A write of a run signals its issue (dispatch://INFRA-4869's plan, decision 6): PutStep sends
// the issue's key on the channel below inside the write's own transaction, so Postgres delivers it
// at the commit, to every Dispatch process that listens, and a write that appends nothing sends
// nothing. The live view of the issue's Legion state moves its runs' revision on it.

// runsSignalChannel is the channel a run's write signals its issue on, by the name the live hub
// listens on, so the rows hold the wire's name whatever the package's constant is called.
const runsSignalChannel = "dispatch_workflow_runs"

// runsSignalQuiet is how long a row waits for a signal that must not come.
const runsSignalQuiet = 500 * time.Millisecond

// listenForRuns opens a connection of its own to pool's database, outside the pool, listening on
// the runs' channel, as the live hub's listener does.
func listenForRuns(t *testing.T, pool *store.Pool) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(t.Context(), pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect a listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(t.Context(), "listen "+runsSignalChannel); err != nil {
		t.Fatalf("listen %s: %v", runsSignalChannel, err)
	}
	return conn
}

// nextRunsSignal is the next notification conn receives within wait, or nil.
func nextRunsSignal(t *testing.T, conn *pgx.Conn, wait time.Duration) *pgconn.Notification {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	notification, err := conn.WaitForNotification(ctx)
	if err != nil {
		if ctx.Err() != nil && t.Context().Err() == nil {
			return nil
		}
		t.Fatalf("wait for a notification on %s: %v", runsSignalChannel, err)
	}
	return notification
}

// requireOneRunsSignal requires conn to receive one notification of issue within 5 seconds, and
// no second one after it.
func requireOneRunsSignal(t *testing.T, conn *pgx.Conn, issue, after string) {
	t.Helper()
	signal := nextRunsSignal(t, conn, 5*time.Second)
	if signal == nil {
		t.Fatalf("after %s, no notification on %s within 5s: an open page would never hear of the run", after, runsSignalChannel)
	}
	if signal.Channel != runsSignalChannel || signal.Payload != issue {
		t.Fatalf("after %s, a notification on %s with payload %q, want %s with the issue %s", after, signal.Channel, signal.Payload, runsSignalChannel, issue)
	}
	if again := nextRunsSignal(t, conn, runsSignalQuiet); again != nil {
		t.Fatalf("after %s, a second notification on %s with payload %q, want one", after, again.Channel, again.Payload)
	}
}

// requireNoRunsSignal requires conn to receive nothing within runsSignalQuiet.
func requireNoRunsSignal(t *testing.T, conn *pgx.Conn, after string) {
	t.Helper()
	if signal := nextRunsSignal(t, conn, runsSignalQuiet); signal != nil {
		t.Fatalf("after %s, a notification on %s with payload %q, want none", after, signal.Channel, signal.Payload)
	}
}

// TestAStepWriteSignalsItsIssueAtCommit holds the signal rows W-1 to W-3, each on a database of
// its own.
func TestAStepWriteSignalsItsIssueAtCommit(t *testing.T) {
	t.Run("W-1 a write that makes a run signals its issue once, at the commit", func(t *testing.T) {
		pool := storetest.Open(t).Pool
		conn := listenForRuns(t, pool)
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the write: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := PutStep(ctx, tx, startOf(implementer(uuid.NewString()), minute(0)), daemon, minute(0)); err != nil {
			t.Fatalf("write the run: %v", err)
		}
		requireNoRunsSignal(t, conn, "a write that has not committed")
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the write: %v", err)
		}
		requireOneRunsSignal(t, conn, "LEG-1", "the write's commit")
	})
	t.Run("W-2 the same write again, which sets nothing, and a write rolled back signal nothing", func(t *testing.T) {
		pool := storetest.Open(t).Pool
		conn := listenForRuns(t, pool)
		id := uuid.NewString()
		put(t, pool, startOf(implementer(id), minute(0)), minute(0))
		requireOneRunsSignal(t, conn, "LEG-1", "the run's first write")
		put(t, pool, startOf(implementer(id), minute(0)), minute(0))
		requireNoRunsSignal(t, conn, "the same write again, which sets nothing")

		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the write rolled back: %v", err)
		}
		if _, err := PutStep(ctx, tx, startOf(implementer(uuid.NewString()), minute(1)), daemon, minute(1)); err != nil {
			t.Fatalf("write the run rolled back: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("roll the write back: %v", err)
		}
		requireNoRunsSignal(t, conn, "a write rolled back")
	})
	t.Run("W-3 a rating written on a retro's record signals its issue", func(t *testing.T) {
		pool := storetest.Open(t).Pool
		conn := listenForRuns(t, pool)
		id := uuid.NewString()
		put(t, pool, startOf(retroOf(id), minute(0)), minute(0))
		requireOneRunsSignal(t, conn, "LEG-1", "the retro's start")
		put(t, pool, rated(retroOf(id), 4, "The tester found the bug."), minute(3))
		requireOneRunsSignal(t, conn, "LEG-1", "the rating of the retro")
	})
}
